package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// truncatingOrigin declares a Content-Length and then sends fewer body bytes
// before closing. A real origin does this when it crashes or is killed
// mid-response, which is not rare.
func newTruncatingOrigin(t *testing.T, declared, sent int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: "+
					itoa(declared)+"\r\nConnection: keep-alive\r\n\r\n")
				c.Write(make([]byte, sent))
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// silentOrigin accepts and reads forever without ever answering.
func newSilentOrigin(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(io.Discard, c); c.Close() }(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// An upstream that sends fewer body bytes than its Content-Length promised must
// not be reported as a successful exchange.
//
// io.Copy over a LimitReader returns a nil error when the source hits EOF
// early: it copied everything there was, which is not the same as everything
// that was promised. If that reads as success, the client is left holding a
// keepalive connection whose framing it and the proxy no longer agree on — the
// client is still waiting for the rest of a body, so the NEXT response on that
// connection is consumed as body bytes. That is response smuggling arriving by
// way of a crashed origin.
func TestTruncatedUpstreamBodyIsNotSuccess(t *testing.T) {
	origin := newTruncatingOrigin(t, 1<<20, 1024) // promises 1 MiB, sends 1 KiB
	r := newRigTo(t, testOpts(), origin.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET /a HTTP/1.1\r\nHost: app.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	c.SetDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response head: %v", err)
	}
	start := time.Now()
	_, err = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("truncated body reported as a complete response")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("proxy held the connection open for %v after a truncated upstream body; "+
			"it treated the short copy as success and kept the client connection alive, "+
			"leaving client and proxy disagreeing about framing", elapsed)
	}
}

// A client that promises a request body and then half-closes without sending it
// must not cost an upstream connection for the whole upstream timeout.
//
// Same nil-error-on-short-copy, other direction. The head and the partial body
// have already gone upstream, so the origin is waiting for bytes that will
// never arrive while the proxy waits for a response that cannot come. Each such
// request pins an upstream connection and a slab slot until UpstreamTimeout,
// and the slab is fixed size — so a handful of them at a cost of one packet
// each exhausts it and every upstream dial fails.
func TestTruncatedRequestBodyFailsFast(t *testing.T) {
	origin := newSilentOrigin(t)
	opts := testOpts() // UpstreamTimeout 20s
	r := newRigTo(t, opts, origin.Addr().String())

	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	head := "POST /x HTTP/1.1\r\nHost: app.test\r\nContent-Length: 100000\r\n\r\n"
	if _, err := io.WriteString(c, head+strings.Repeat("x", 5)); err != nil {
		t.Fatal(err)
	}
	// Half-close: the body ends here, well short of what was promised.
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 64))
	elapsed := time.Since(start)

	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("proxy still waiting after %v; a truncated request body holds an "+
			"upstream connection and a slab slot until UpstreamTimeout (%v)",
			elapsed, opts.UpstreamTimeout)
	}
}

// continueOrigin answers Expect: 100-continue the way an origin should — an
// interim 100, then it reads the body, then the real response.
func newContinueOrigin(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
				io.Copy(io.Discard, req.Body)
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// A client sending Expect: 100-continue waits for the interim response before
// sending its body. The proxy must therefore relay that interim response before
// it blocks reading the body.
func TestExpect100ContinueIsRelayedBeforeTheBody(t *testing.T) {
	origin := newContinueOrigin(t)
	r := newRigTo(t, testOpts(), origin.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	head := "POST /x HTTP/1.1\r\nHost: app.test\r\nContent-Length: 10\r\n" +
		"Expect: 100-continue\r\n\r\n"
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}

	// The body is deliberately withheld, exactly as a conforming client does.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("no interim response: the proxy is blocked reading a body the " +
			"client is not allowed to send until it sees 100 Continue")
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, "HTTP/1.1 100") {
		t.Fatalf("first response was %q, want 100 Continue", got)
	}
}

// An origin may answer Expect: 100-continue with a final status instead of an
// interim one — 417, or an auth rejection it can make before paying for the
// body. The proxy must relay that and stop, not go on waiting for a body the
// client will now never send.
func TestExpect100RejectedByOrigin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n")
			}(c)
		}
	}()

	r := newRigTo(t, testOpts(), ln.Addr().String())
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	head := "POST /x HTTP/1.1\r\nHost: app.test\r\nContent-Length: 10\r\n" +
		"Expect: 100-continue\r\n\r\n"
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no response to a rejected Expect: %v", err)
	}
	if resp.StatusCode != 417 {
		t.Fatalf("status %d, want 417", resp.StatusCode)
	}
	if !resp.Close {
		t.Error("the response kept the client connection alive, but the client " +
			"may still send the body it withheld and those bytes would be parsed " +
			"as the next request")
	}
}
