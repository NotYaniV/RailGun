package httpx

// Fuzz targets for the three parsers. Each target checks invariants that must
// hold for every input rather than for a list of inputs someone thought of:
//
//   - no panic, and a byte count that is zero on error and HeadLen on success;
//   - restartability: every strict prefix of an accepted head is ErrIncomplete,
//     because the caller re-parses as bytes arrive and must never act early;
//   - round trip: what the serializer emits re-parses to the same meaning;
//   - agreement with net/http on framing, since a proxy and its origin
//     disagreeing about where a message ends is the smuggling class.
//
// The differential half uses net/http only in this file, which is a _test file
// and so does not breach the no-I/O import rule that import_test.go enforces.
//
// Run: go test ./internal/httpx -run '^$' -fuzz FuzzRequest -fuzztime 60s

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"testing"
)

// withSlack copies b into a buffer with spare capacity so len < cap and the
// parser's head-too-large check does not fire on what is merely a short read.
func withSlack(b []byte) []byte {
	buf := make([]byte, len(b), len(b)+1)
	copy(buf, b)
	return buf
}

var requestSeeds = []string{
	"GET / HTTP/1.1\r\nHost: a\r\n\r\n",
	"POST /x HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello",
	"POST /x HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
	"POST /x HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: gzip, chunked\r\n\r\n",
	"GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n",
	"GET /ws HTTP/1.1\r\nHost: a\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: a\r\nConnection: close, X-Foo\r\nX-Foo: 1\r\n\r\n",
	"POST / HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 0\r\n\r\n",
	"CONNECT a:443 HTTP/1.1\r\nHost: a:443\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\n",
}

func FuzzRequest(f *testing.F) {
	for _, s := range requestSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var r Request
		n, err := r.Parse(withSlack(in))
		if err != nil {
			if n != 0 {
				t.Fatalf("error %v with n=%d, want 0", err, n)
			}
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("error %T is not *Error", err)
			}
			return
		}
		if n <= 0 || n > len(in) || n != r.HeadLen {
			t.Fatalf("n=%d HeadLen=%d len=%d", n, r.HeadLen, len(in))
		}

		// Restartability.
		for k := 0; k < n; k++ {
			var p Request
			if _, err := p.Parse(withSlack(in[:k])); err != ErrIncomplete {
				t.Fatalf("prefix %d/%d of accepted head: got %v, want ErrIncomplete", k, n, err)
			}
		}

		// Round trip through the serializer. A request with no Host is never
		// forwarded (Route rejects an empty host), so it is not checked.
		if r.Host == nil {
			return
		}
		out := AppendRequestHead(nil, &r, r.Close)
		var rt Request
		if _, err := rt.Parse(withSlack(out)); err != nil {
			t.Fatalf("serialized head does not re-parse: %v\nin:  %q\nout: %q", err, in[:n], out)
		}
		if !bytes.Equal(rt.Method, r.Method) || !bytes.Equal(rt.Target, r.Target) ||
			!bytes.Equal(rt.Host, r.Host) || rt.Chunked != r.Chunked ||
			rt.ContentLength != r.ContentLength {
			t.Fatalf("round trip changed meaning\nin:  %q\nout: %q", in[:n], out)
		}
		if teVal := r.Get(nameTransferEncoding); r.Chunked && !EqualFold(trimOWS(teVal), tokChunked) {
			// The body still carries every coding the client applied; the
			// origin is told only about the last one.
			t.Fatalf("Transfer-Encoding %q forwarded as plain chunked\nout: %q", teVal, out)
		}

		// Differential against net/http on framing. Only checked when both
		// accept: rejecting more than net/http is a choice, accepting the
		// same bytes and framing them differently is a desync.
		hr, herr := http.ReadRequest(bufio.NewReader(bytes.NewReader(in[:n])))
		if herr != nil {
			return
		}
		goChunked := len(hr.TransferEncoding) > 0 && hr.TransferEncoding[len(hr.TransferEncoding)-1] == "chunked"
		if goChunked != r.Chunked {
			t.Fatalf("chunked: ours=%v net/http=%v (TE %q)\nin: %q", r.Chunked, goChunked, hr.TransferEncoding, in[:n])
		}
		if !r.Chunked {
			ours := r.ContentLength
			if ours < 0 {
				ours = 0
			}
			if hr.ContentLength != ours {
				t.Fatalf("content-length: ours=%d net/http=%d\nin: %q", r.ContentLength, hr.ContentLength, in[:n])
			}
		}
	})
}

var responseSeeds = []string{
	"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi",
	"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
	"HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip, chunked\r\n\r\n",
	"HTTP/1.1 204 No Content\r\n\r\n",
	"HTTP/1.0 200 OK\r\n\r\nbody until close",
	"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
	"HTTP/1.1 200\r\nContent-Length: 0\r\n\r\n",
}

func FuzzResponse(f *testing.F) {
	for _, s := range responseSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var r Response
		n, err := r.Parse(withSlack(in))
		if err != nil {
			if n != 0 {
				t.Fatalf("error %v with n=%d, want 0", err, n)
			}
			return
		}
		if n <= 0 || n > len(in) || n != r.HeadLen {
			t.Fatalf("n=%d HeadLen=%d len=%d", n, r.HeadLen, len(in))
		}
		for k := 0; k < n; k++ {
			var p Response
			if _, err := p.Parse(withSlack(in[:k])); err != ErrIncomplete {
				t.Fatalf("prefix %d/%d of accepted head: got %v, want ErrIncomplete", k, n, err)
			}
		}

		// 1xx and tunnel responses go through AppendTunnelResponseHead, not
		// this serializer (relayResponse).
		if f, _ := r.Framing(nil); r.Informational() || f == BodyTunnel {
			return
		}
		out := AppendResponseHead(nil, &r, r.Close)
		var rt Response
		if _, err := rt.Parse(withSlack(out)); err != nil {
			t.Fatalf("serialized head does not re-parse: %v\nin:  %q\nout: %q", err, in[:n], out)
		}
		for _, m := range []string{"GET", "HEAD"} {
			f1, l1 := r.Framing([]byte(m))
			f2, l2 := rt.Framing([]byte(m))
			if f1 != f2 || l1 != l2 {
				t.Fatalf("%s framing changed across serialize: %v/%d -> %v/%d\nin:  %q\nout: %q", m, f1, l1, f2, l2, in[:n], out)
			}
		}
		if teVal := r.Get(nameTransferEncoding); r.Chunked && !EqualFold(trimOWS(teVal), tokChunked) {
			t.Fatalf("Transfer-Encoding %q forwarded as plain chunked\nout: %q", teVal, out)
		}
	})
}

var chunkSeeds = []string{
	"0\r\n\r\n",
	"5\r\nhello\r\n0\r\n\r\n",
	"5;ext=1\r\nhello\r\n0\r\nTrailer: x\r\n\r\n",
	"A\r\n0123456789\r\n0\r\n\r\nGET / HTTP/1.1\r\n",
	"5 \r\nhello\r\n0\r\n\r\n",
}

func FuzzChunkScanner(f *testing.F) {
	for _, s := range chunkSeeds {
		f.Add([]byte(s), uint8(1))
	}
	f.Fuzz(func(t *testing.T, in []byte, step uint8) {
		var whole ChunkScanner
		n, done, err := whole.Scan(in)

		// Splitting the same bytes across reads must not change the outcome.
		// Fed in pieces, consumption stops at the first error or at done.
		sz := int(step%16) + 1
		var split ChunkScanner
		sn, sdone := 0, false
		var serr error
		for off := 0; off < len(in) && !sdone && serr == nil; off += sz {
			end := min(off+sz, len(in))
			k, d, e := split.Scan(in[off:end])
			sn += k
			sdone, serr = d, e
		}
		if (err == nil) != (serr == nil) || done != sdone || (done && n != sn) {
			t.Fatalf("whole: n=%d done=%v err=%v; split(%d): n=%d done=%v err=%v\nin: %q",
				n, done, err, sz, sn, sdone, serr, in)
		}
		if !done {
			return
		}

		// Whatever we call a complete body, net/http must decode cleanly.
		// It stops at the zero-size chunk and leaves the trailer, so only
		// the chunk data is compared, not the byte count.
		if _, err := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(in[:n]))); err != nil {
			t.Fatalf("we accept a body net/http rejects: %v\nin: %q", err, in[:n])
		}
	})
}
