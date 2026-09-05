package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/proxy"
)

// lateOverrunOrigin answers each request correctly, then — after a delay long
// enough for the proxy to have returned the connection to its pool — writes an
// unsolicited extra response onto the same connection.
//
// The delay is what makes this different from the same-segment overrun the
// end-of-exchange check already handles. Those bytes have arrived by the time
// the exchange finishes and can be seen there. These arrive afterwards, into
// the socket buffer of a connection that is already idle in the pool, where
// nothing in the netpoll design is looking.
func lateOverrunOrigin(t *testing.T, delay time.Duration) net.Listener {
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
				for {
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					if _, err := io.WriteString(c,
						"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nfirst"); err != nil {
						return
					}
					time.Sleep(delay)
					// Nobody asked for this.
					if _, err := io.WriteString(c,
						"HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nINJECT"); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// TestVerifyPooledConnsCatchesLateOverrun is the evidence for experiment E3.
//
// It documents a real limit of the Approach A design rather than only testing a
// flag. The end-of-exchange leftover check cannot see bytes that arrive after
// the exchange ends, because a netpoll proxy has nothing watching an idle
// pooled connection — and giving it something to watch means a goroutine per
// pooled connection, which is the cost this architecture exists to avoid.
//
// Verifying at acquire time closes the window from the other side, for one
// non-blocking syscall per pooled reuse.
func TestVerifyPooledConnsCatchesLateOverrun(t *testing.T) {
	o := testOpts()
	o.VerifyPooledConns = true
	r := newRigTo(t, o, lateOverrunOrigin(t, 40*time.Millisecond).Addr().String())
	c := r.client()

	// First request primes the pool. The origin's unsolicited response lands
	// after this returns.
	if _, body := r.get(c, "/a"); string(body) != "first" {
		t.Fatalf("first response body %q, want %q", body, "first")
	}
	time.Sleep(120 * time.Millisecond)

	// Second request must NOT be answered with the injected response.
	resp, body := r.get(c, "/b")
	if resp.StatusCode != 200 {
		t.Fatalf("second request status %d", resp.StatusCode)
	}
	if string(body) == "INJECT" {
		t.Fatal("second request was answered with the response the origin volunteered after the first exchange; the acquire-time check did not catch it")
	}
	if string(body) != "first" {
		t.Fatalf("second response body %q, want %q", body, "first")
	}

	if n := r.srv.DirtyPooledConns(); n == 0 {
		t.Error("no pooled connection was reported dirty, so this test passed without exercising the check it exists for")
	} else {
		t.Logf("discarded %d dirty pooled connection(s)", n)
	}
}

// TestVerifyPooledConnsOffIsTheDefault pins that E3 is opt-in. The measurement
// it exists for is a paired comparison, and an experiment arm that silently
// became the default would destroy the baseline it is measured against.
func TestVerifyPooledConnsOffIsTheDefault(t *testing.T) {
	if proxy.DefaultOptions().VerifyPooledConns {
		t.Fatal("VerifyPooledConns defaults to on; it costs a syscall per request and must be opted into")
	}
}

// TestVerifyPooledConnsKeepsHealthyConnectionsPooled checks the check does not
// pay for itself by throwing away perfectly good connections. If it discarded
// aggressively it would dial on every request, which would look like a latency
// regression in E3 and be blamed on the syscall rather than on the policy.
func TestVerifyPooledConnsKeepsHealthyConnectionsPooled(t *testing.T) {
	o := testOpts()
	o.VerifyPooledConns = true
	r := newRig(t, o, echoHandler())
	c := r.client()

	const n = 12
	for i := 0; i < n; i++ {
		if resp, body := r.get(c, "/small"); resp.StatusCode != 200 || string(body) != "hello" {
			t.Fatalf("request %d: %d %q", i, resp.StatusCode, body)
		}
	}

	var dials, reuses uint64
	for _, st := range r.srv.PoolStats() {
		dials += st.Dials
		reuses += st.Reuses
	}
	if reuses == 0 {
		t.Fatalf("no reuse across %d requests with verification on (dials=%d); the check is discarding healthy connections", n, dials)
	}
	if d := r.srv.DirtyPooledConns(); d != 0 {
		t.Errorf("%d healthy connections reported dirty", d)
	}
	t.Logf("%d requests with verification on: %d dials, %d reuses, %d dirty", n, dials, reuses, r.srv.DirtyPooledConns())
}

// TestLateOverrunSurvivesWithoutVerification records the limitation honestly:
// with E3 off, the late-arriving overrun DOES reach the next exchange. This is
// the control arm, and it exists so nobody later reads the fix above as
// covering a case it does not.
func TestLateOverrunSurvivesWithoutVerification(t *testing.T) {
	o := testOpts()
	o.VerifyPooledConns = false
	r := newRigTo(t, o, lateOverrunOrigin(t, 40*time.Millisecond).Addr().String())
	c := r.client()

	if _, body := r.get(c, "/a"); string(body) != "first" {
		t.Fatalf("first response body %q", body)
	}
	time.Sleep(120 * time.Millisecond)

	resp, body := r.get(c, "/b")
	got := string(body)
	// Not asserted as a failure: this documents current behaviour. If it ever
	// starts returning "first" reliably, the netpoll design has gained
	// something and this test should become an assertion.
	t.Logf("without acquire-time verification the second request returned status %d body %q (%s)",
		resp.StatusCode, got,
		map[bool]string{true: "the injected response reached the client", false: "not reproduced on this run"}[got == "INJECT"])
}
