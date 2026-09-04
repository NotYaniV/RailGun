package httpx

import (
	"strings"
	"testing"
)

func req(s string) []byte { return []byte(s) }

// TestParseRequestSmuggling is the security spine of this package. Every case
// here is a published request-smuggling primitive or a framing ambiguity that
// enables one. A parser that accepts any of them is not merely lenient: it
// desynchronizes the proxy from its origin, and a desynchronized pooled
// connection serves one client's body as another client's request.
//
// Per design spec section 5.3 these defenses exist before any benchmark runs.
// A proxy that is fast and smuggleable is not a result.
func TestParseRequestSmuggling(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *Error
	}{
		{
			name: "CL.TE both framing headers",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n",
			want: ErrLenAndTransfer,
		},
		{
			name: "TE.CL reversed order still rejected",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nContent-Length: 6\r\n\r\n",
			want: ErrLenAndTransfer,
		},
		{
			name: "duplicate Content-Length with differing values",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nContent-Length: 5\r\n\r\n",
			want: ErrDupContentLen,
		},
		{
			name: "duplicate Content-Length with identical values",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nContent-Length: 6\r\n\r\n",
			want: ErrDupContentLen,
		},
		{
			name: "Content-Length as a comma list",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6, 6\r\n\r\n",
			want: ErrBadContentLen,
		},
		{
			name: "signed Content-Length",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: +6\r\n\r\n",
			want: ErrBadContentLen,
		},
		{
			name: "obs-fold continuation line",
			in:   "GET / HTTP/1.1\r\nHost: a\r\nX-Thing: one\r\n two\r\n\r\n",
			want: ErrObsFold,
		},
		{
			name: "space before colon in field name",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nContent-Length : 6\r\n\r\n",
			want: ErrBadHeaderName,
		},
		{
			name: "non-token byte in field name",
			in:   "GET / HTTP/1.1\r\nHost: a\r\nX\x00Thing: v\r\n\r\n",
			want: ErrBadHeaderName,
		},
		{
			name: "bare LF terminating a header line",
			in:   "GET / HTTP/1.1\r\nHost: a\nContent-Length: 6\r\n\r\n",
			want: ErrBareLF,
		},
		{
			name: "bare LF terminating the request line",
			in:   "GET / HTTP/1.1\nHost: a\r\n\r\n",
			want: ErrBareLF,
		},
		{
			name: "bare CR inside a header line",
			in:   "GET / HTTP/1.1\r\nHost: a\rb\r\n\r\n",
			want: ErrBareCR,
		},
		{
			name: "Transfer-Encoding whose last coding is not chunked",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked, gzip\r\n\r\n",
			want: ErrBadTransferEnc,
		},
		{
			name: "Transfer-Encoding: identity",
			in:   "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: identity\r\n\r\n",
			want: ErrBadTransferEnc,
		},
		{
			name: "two Host headers",
			in:   "GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
			want: ErrDupHost,
		},
		{
			name: "HTTP/1.1 with no Host",
			in:   "GET / HTTP/1.1\r\n\r\n",
			want: ErrMissingHost,
		},
		{
			name: "two spaces in the request line",
			in:   "GET  / HTTP/1.1\r\nHost: a\r\n\r\n",
			want: ErrBadRequestLine,
		},
		{
			name: "tab as request-line separator",
			in:   "GET\t/ HTTP/1.1\r\nHost: a\r\n\r\n",
			want: ErrBadRequestLine,
		},
		{
			name: "unsupported version",
			in:   "GET / HTTP/2.0\r\nHost: a\r\n\r\n",
			want: ErrBadVersion,
		},
		{
			name: "control byte in header value",
			in:   "GET / HTTP/1.1\r\nHost: a\r\nX-Thing: bad\x01value\r\n\r\n",
			want: ErrBadHeaderValue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r Request
			n, err := r.Parse(req(tc.in))
			if err != tc.want {
				t.Fatalf("got (%d, %v), want error %v", n, err, tc.want)
			}
			if n != 0 {
				t.Errorf("failed parse consumed %d bytes, want 0", n)
			}
		})
	}
}

// TestParseRequestTooManyHeaders checks the trust-boundary limit from spec
// 5.3: oversized header blocks are rejected with 431, never grown into.
func TestParseRequestTooManyHeaders(t *testing.T) {
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\nHost: a\r\n")
	for i := 0; i < MaxHeaders+5; i++ {
		b.WriteString("X-Pad: v\r\n")
	}
	b.WriteString("\r\n")

	var r Request
	if _, err := r.Parse([]byte(b.String())); err != ErrTooManyHeaders {
		t.Fatalf("got %v, want ErrTooManyHeaders", err)
	}
	if ErrTooManyHeaders.Status != 431 {
		t.Fatalf("ErrTooManyHeaders.Status = %d, want 431", ErrTooManyHeaders.Status)
	}
}

func TestParseRequestIncomplete(t *testing.T) {
	full := "GET /x HTTP/1.1\r\nHost: a\r\n\r\n"
	for i := 0; i < len(full)-1; i++ {
		var r Request
		buf := make([]byte, i, len(full))
		copy(buf, full[:i])
		if _, err := r.Parse(buf); err != ErrIncomplete {
			// A prefix cut mid-CRLF must still be "need more", never a
			// framing error: the proxy would otherwise reject a request that
			// merely arrived in two TCP segments.
			t.Fatalf("prefix len %d: got %v, want ErrIncomplete", i, err)
		}
	}
}

func TestParseRequestOK(t *testing.T) {
	in := "POST /a/b?c=d HTTP/1.1\r\n" +
		"Host: example.com:8080\r\n" +
		"Content-Length: 4\r\n" +
		"Connection: keep-alive\r\n" +
		"X-Trace: 1\r\n" +
		"\r\nbody"

	var r Request
	n, err := r.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n != len(in)-4 {
		t.Errorf("consumed %d, want %d", n, len(in)-4)
	}
	if string(r.Method) != "POST" {
		t.Errorf("Method = %q", r.Method)
	}
	if string(r.Target) != "/a/b?c=d" {
		t.Errorf("Target = %q", r.Target)
	}
	if r.Minor != 1 {
		t.Errorf("Minor = %d", r.Minor)
	}
	if string(r.Host) != "example.com:8080" {
		t.Errorf("Host = %q", r.Host)
	}
	if string(r.HostNoPort()) != "example.com" {
		t.Errorf("HostNoPort = %q", r.HostNoPort())
	}
	if r.ContentLength != 4 {
		t.Errorf("ContentLength = %d", r.ContentLength)
	}
	if r.Close {
		t.Error("Close = true, want false")
	}
	if got := len(r.Headers()); got != 4 {
		t.Errorf("header count = %d, want 4", got)
	}
	if string(r.Get([]byte("x-trace"))) != "1" {
		t.Errorf("Get(x-trace) = %q", r.Get([]byte("x-trace")))
	}
}

func TestHostNoPortIPv6(t *testing.T) {
	cases := map[string]string{
		"example.com:8080":  "example.com",
		"example.com":       "example.com",
		"[::1]:8080":        "[::1]",
		"[fe80::1%25eth0]":  "[fe80::1%25eth0]",
		"[2001:db8::1]:443": "[2001:db8::1]",
	}
	for in, want := range cases {
		r := Request{Host: []byte(in)}
		if got := string(r.HostNoPort()); got != want {
			t.Errorf("HostNoPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTP10DefaultsToClose(t *testing.T) {
	var r Request
	if _, err := r.Parse(req("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if !r.Close {
		t.Error("HTTP/1.0 without keep-alive must default to Close")
	}

	r.Reset()
	if _, err := r.Parse(req("GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if r.Close {
		t.Error("HTTP/1.0 with keep-alive must not default to Close")
	}
}

func TestUpgradeDetection(t *testing.T) {
	var r Request
	in := "GET /ws HTTP/1.1\r\nHost: a\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	if _, err := r.Parse(req(in)); err != nil {
		t.Fatal(err)
	}
	if string(r.Upgrade) != "websocket" {
		t.Errorf("Upgrade = %q", r.Upgrade)
	}
	if !r.Bidirectional() {
		t.Error("upgrade request must be Bidirectional")
	}

	// Upgrade without Connection: upgrade is not an upgrade. Treating it as
	// one would let a client turn an ordinary request into a tunnel.
	r.Reset()
	in = "GET /ws HTTP/1.1\r\nHost: a\r\nUpgrade: websocket\r\n\r\n"
	if _, err := r.Parse(req(in)); err != nil {
		t.Fatal(err)
	}
	if r.Upgrade != nil {
		t.Errorf("Upgrade = %q, want nil without Connection: upgrade", r.Upgrade)
	}
}

// TestParseRequestNoAlloc is the performance spine, and it is a correctness
// test in disguise. The entire premise of writing a parser instead of using
// net/http is that this number is zero.
func TestParseRequestNoAlloc(t *testing.T) {
	buf := req("GET /index.html HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"User-Agent: wrk2\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n\r\n")
	var r Request

	got := testing.AllocsPerRun(200, func() {
		if _, err := r.Parse(buf); err != nil {
			t.Fatal(err)
		}
		_, _ = r.Get([]byte("user-agent")), r.HostNoPort()
	})
	if got != 0 {
		t.Fatalf("Parse allocated %v times per run, want 0", got)
	}
}

func BenchmarkParseRequest(b *testing.B) {
	buf := req("GET /index.html HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"User-Agent: wrk2\r\n" +
		"Accept: */*\r\n" +
		"Accept-Encoding: gzip\r\n" +
		"Connection: keep-alive\r\n\r\n")
	var r Request
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Parse(buf); err != nil {
			b.Fatal(err)
		}
	}
}
