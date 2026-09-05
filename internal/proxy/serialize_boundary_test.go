package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// echoRequestHandler reports back exactly what the origin's own parser saw, so
// a test can compare what the client sent against what came out the far side of
// httpx's serializer.
//
// net/http is the point: it is an independent, strict implementation of the
// same specification. Bug 7 — the serializer stripping the two headers that
// make an upgrade an upgrade — was invisible to every test that checked httpx
// against itself and died the moment a real parser was on the other end.
func echoRequestHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		fmt.Fprintf(&b, "method=%s\n", r.Method)
		fmt.Fprintf(&b, "uri=%s\n", r.RequestURI)
		fmt.Fprintf(&b, "host=%s\n", r.Host)
		keys := make([]string, 0, len(r.Header))
		for k := range r.Header {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, v := range r.Header[k] {
				fmt.Fprintf(&b, "hdr=%s: %s\n", k, v)
			}
		}
		io.WriteString(w, b.String())
	})
}

// rawExchange sends one raw request and returns the response body.
func rawExchange(t *testing.T, addr, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	return string(body)
}

func TestSerializedRequestReachesARealOriginIntact(t *testing.T) {
	r := newRig(t, testOpts(), echoRequestHandler())

	got := rawExchange(t, r.addr,
		"GET /a/b%2Fc?q=1&q=2&empty=&x=%20 HTTP/1.1\r\n"+
			"Host: app.test\r\n"+
			"X-Custom: one\r\n"+
			"X-Custom: two\r\n"+
			"X-Spaces:    padded   \r\n"+
			"Accept: */*\r\n"+
			"\r\n")

	for _, want := range []string{
		"method=GET",
		// The target must arrive byte-for-byte. A proxy that normalises
		// percent-encoding or collapses the query differs from its origin about
		// what resource was requested, which is how path-based authorisation
		// gets bypassed.
		"uri=/a/b%2Fc?q=1&q=2&empty=&x=%20",
		"host=app.test",
		// Duplicate field lines must stay two lines, not be joined.
		"hdr=X-Custom: one",
		"hdr=X-Custom: two",
		// Optional whitespace around the value is not part of it.
		"hdr=X-Spaces: padded",
		"hdr=Accept: */*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("origin did not see %q\nsaw:\n%s", want, got)
		}
	}
}

// RFC 9110 §7.6.1: a proxy must remove every header field named in the
// Connection header before forwarding, not just Connection itself.
//
// Those fields are addressed to this hop. Passing them on lets a client reach
// through the proxy and set headers on the proxy-to-origin connection, which is
// the mechanism behind several request smuggling chains.
func TestConnectionNamedHeadersAreNotForwarded(t *testing.T) {
	r := newRig(t, testOpts(), echoRequestHandler())

	got := rawExchange(t, r.addr,
		"GET / HTTP/1.1\r\n"+
			"Host: app.test\r\n"+
			"Connection: keep-alive, X-Hop-Secret\r\n"+
			"X-Hop-Secret: reached-the-origin\r\n"+
			"X-Kept: yes\r\n"+
			"\r\n")

	if strings.Contains(got, "X-Hop-Secret") {
		t.Errorf("a header named in Connection reached the origin; the client "+
			"can set headers on the proxy-to-origin connection\nsaw:\n%s", got)
	}
	if !strings.Contains(got, "hdr=X-Kept: yes") {
		t.Errorf("an ordinary header was dropped\nsaw:\n%s", got)
	}
	// The proxy states its OWN Connection value to the origin, which is
	// correct — that header describes the proxy-to-origin hop. What must not
	// survive is the client's token list.
	if strings.Contains(got, "X-Hop-Secret") {
		t.Errorf("the client's Connection token list survived\nsaw:\n%s", got)
	}
}

// The same rule in the other direction, which is where this codebase has
// repeatedly had one side right and the other wrong: the copy arm checked for a
// short body and the splice arm did not, request truncation and response
// truncation were separate bugs. An origin naming a header in its Connection
// response header is asking for that header to stop at this hop.
func TestConnectionNamedResponseHeadersAreNotForwarded(t *testing.T) {
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
				for {
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					io.WriteString(c, "HTTP/1.1 200 OK\r\n"+
						"Content-Length: 2\r\n"+
						"Connection: keep-alive, X-Origin-Hop\r\n"+
						"X-Origin-Hop: leaked\r\n"+
						"X-Kept: yes\r\n\r\nok")
				}
			}(c)
		}
	}()

	r := newRigTo(t, testOpts(), ln.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: app.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if v := resp.Header.Get("X-Origin-Hop"); v != "" {
		t.Errorf("a response header the origin named in Connection reached the "+
			"client: X-Origin-Hop: %s", v)
	}
	if resp.Header.Get("X-Kept") != "yes" {
		t.Errorf("an ordinary response header was dropped: %v", resp.Header)
	}
}
