package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A pipelined request must survive an upstream retry on the request in front of
// it. The retry replays the head upstream; it must not replay the buffer
// compaction that dropped that head from the downstream read buffer, because
// the bytes now at the front of that buffer are the NEXT client request.
func TestPipelinedRequestSurvivesUpstreamRetry(t *testing.T) {
	origin := newTreacherousOrigin(t)
	r := newRigTo(t, testOpts(), origin.ln.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)

	req := "GET /a HTTP/1.1\r\nHost: app.test\r\n\r\n"

	// Warm the pool so the next request lands on a connection the origin has
	// already closed, which is what forces the retry.
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	warm, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("warmup response: %v", err)
	}
	io.Copy(io.Discard, warm.Body)
	warm.Body.Close()

	// Two requests in ONE write: the first retries, the second is sitting in
	// the proxy's read buffer while it happens.
	if _, err := io.WriteString(c, req+req); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("pipelined response %d: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "ok" {
			t.Fatalf("pipelined response %d: %d %q, want 200 \"ok\"", i+1, resp.StatusCode, body)
		}
	}
}

// A client that sends part of a request head and then stalls must be bounded by
// ReadTimeout, not by the much longer keepalive IdleTimeout. Otherwise one byte
// buys a connection and a pooled connState for the whole idle window.
func TestPartialHeadIsBoundedByReadTimeout(t *testing.T) {
	origin := newTreacherousOrigin(t)
	opts := testOpts()
	opts.IdleTimeout = 30 * time.Second
	opts.ReadTimeout = 250 * time.Millisecond
	r := newRigTo(t, opts, origin.ln.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// One byte of a request head, then silence.
	if _, err := io.WriteString(c, "G"); err != nil {
		t.Fatal(err)
	}

	// The proxy may answer 400 or simply close; either ends the connection. What
	// matters is WHEN. Bounded by ReadTimeout it happens in about 250ms; bounded
	// by the idle timeout it would not happen for 30 seconds.
	start := time.Now()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	_, err = c.Read(buf)
	elapsed := time.Since(start)

	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("proxy held the stalled connection past %v; the head read is "+
			"still bounded by IdleTimeout (%v) rather than ReadTimeout (%v)",
			elapsed, opts.IdleTimeout, opts.ReadTimeout)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("stalled connection ended after %v, want about %v", elapsed, opts.ReadTimeout)
	}
	t.Logf("stalled client ended after %v (ReadTimeout %v, IdleTimeout %v)",
		elapsed, opts.ReadTimeout, opts.IdleTimeout)
}
