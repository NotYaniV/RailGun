package proxy_test

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/metrics"
)

// TestClientFaultIsNotBlamedOnTheUpstream pins error attribution.
//
// This is a benchmark-integrity test rather than a correctness one, and it is
// worth stating why it earns its place. The comparison against NGINX publishes
// an upstream error rate. A client that truncates its own request body is not
// an upstream failure, and counting it as one inflates the number this project
// exists to report — in the direction that makes Railgun look worse, but wrong
// either way. It also answers the client 502, blaming an origin that did
// nothing wrong.
//
// The failure mode is invisible on the rig, because wrk2 never truncates. That
// is exactly why it needs a test: nothing in the benchmark run would ever
// exercise it, and the number would simply be quietly wrong if a real client
// population ever hit it.
func TestClientFaultIsNotBlamedOnTheUpstream(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	before := metrics.Scrape()

	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	// Promise 100000 bytes, send five, then half-close so the proxy sees EOF.
	head := "POST /echo HTTP/1.1\r\nHost: app.test\r\nContent-Length: 100000\r\n\r\n"
	if _, err := io.WriteString(c, head+"hello"); err != nil {
		t.Fatal(err)
	}
	if tc, ok := c.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}

	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	got := string(buf[:n])
	if n == 0 {
		t.Fatal("proxy sent no response to a truncated request body")
	}
	if !strings.Contains(got, "400") {
		t.Errorf("response was %q; a client that truncates its own body should get 400, not a 502 blaming the origin",
			strings.SplitN(got, "\r\n", 2)[0])
	}

	after := metrics.Scrape()
	if d := after.Errors[metrics.ErrBadGateway] - before.Errors[metrics.ErrBadGateway]; d != 0 {
		t.Errorf("ErrBadGateway rose by %d for a client-caused failure; upstream error rate is a published number and this is not an upstream error", d)
	}
}

// TestUpstreamFaultIsStillBlamedOnTheUpstream is the other half. Without it the
// test above passes trivially if error attribution stops working entirely.
func TestUpstreamFaultIsStillBlamedOnTheUpstream(t *testing.T) {
	// An upstream address nothing is listening on: dialing it genuinely fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	r := newRigTo(t, testOpts(), dead)
	before := metrics.Scrape()

	resp, _ := r.get(r.client(), "/small")
	if resp.StatusCode == 200 {
		t.Fatalf("request to a dead upstream returned 200")
	}

	after := metrics.Scrape()
	dialErrs := after.Errors[metrics.ErrUpstreamDial] - before.Errors[metrics.ErrUpstreamDial]
	badGw := after.Errors[metrics.ErrBadGateway] - before.Errors[metrics.ErrBadGateway]
	if dialErrs+badGw == 0 {
		t.Error("a genuine upstream failure incremented no upstream error counter")
	}
	t.Log(fmt.Sprintf("dead upstream: status %d, dial errors +%d, bad gateway +%d",
		resp.StatusCode, dialErrs, badGw))
}
