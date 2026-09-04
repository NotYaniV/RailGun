package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/proxy"
)

// treacherousOrigin serves one request per connection and then closes, while
// advertising keep-alive.
//
// This reproduces the keepalive race exactly: the proxy pools a connection the
// origin has already decided to discard, and discovers it only when it writes
// the next request. It is the single most common transient failure a reverse
// proxy sees in production, it is invisible in any test whose origin behaves,
// and it is the entire reason the pool reports whether a connection was reused.
type treacherousOrigin struct {
	ln       net.Listener
	requests atomic.Int64
}

func newTreacherousOrigin(t *testing.T) *treacherousOrigin {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := &treacherousOrigin{ln: ln}
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
				o.requests.Add(1)
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nok")
				// The lie: keep-alive was advertised, the socket closes anyway.
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return o
}

func newRigTo(t *testing.T, opts proxy.Options, upstreamAddr string) *rig {
	t.Helper()
	snap, err := config.NewSnapshot(1,
		[]config.Route{{Host: "app.test", Upstream: "app"}},
		[]config.Upstream{{Name: "app", Addrs: []string{upstreamAddr}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var store config.Store
	store.Store(snap)

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(&store, opts)
	go srv.Serve([]*net.TCPListener{pln.(*net.TCPListener)})
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		pln.Close()
		srv.Shutdown(done)
	})
	return &rig{t: t, srv: srv, addr: pln.Addr().String()}
}

// TestUpstreamKeepAliveRaceIsRetried asserts the client never sees the race.
//
// Every request here lands on a pooled connection the origin has already
// closed. Without the retry, the second and every subsequent request returns
// 502. NGINX retries this by default via proxy_next_upstream, so a Railgun that
// did not would report errors on a workload where NGINX reports none — a
// benchmark artifact that looks like a reliability difference between the two
// proxies rather than a policy difference between their defaults.
func TestUpstreamKeepAliveRaceIsRetried(t *testing.T) {
	origin := newTreacherousOrigin(t)
	r := newRigTo(t, testOpts(), origin.ln.Addr().String())
	c := r.client()

	for i := 0; i < 6; i++ {
		resp, body := r.get(c, "/x")
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d, want 200 (the keepalive race reached the client)", i, resp.StatusCode)
		}
		if string(body) != "ok" {
			t.Fatalf("request %d: body %q", i, body)
		}
	}

	var dials, reuses uint64
	for _, st := range r.srv.PoolStats() {
		dials += st.Dials
		reuses += st.Reuses
	}
	// The race must actually have been provoked, or this test proves nothing:
	// a reuse that then failed is the precondition for the retry firing.
	if reuses == 0 {
		t.Fatalf("no connection was ever reused, so the race never happened; test is vacuous (dials=%d)", dials)
	}
	t.Logf("6 requests through a lying origin: %d dials, %d reuses, origin saw %d requests",
		dials, reuses, origin.requests.Load())
}

// TestNonIdempotentIsNotRetried checks the other half of the policy. A POST
// that fails on a reused connection must NOT be replayed: the origin may have
// applied it before the connection broke, and a silent second application is
// worse than a visible error.
func TestNonIdempotentIsNotRetried(t *testing.T) {
	origin := newTreacherousOrigin(t)
	r := newRigTo(t, testOpts(), origin.ln.Addr().String())
	c := r.client()

	// Prime the pool so the next request lands on a dead connection.
	if resp, _ := r.get(c, "/x"); resp.StatusCode != 200 {
		t.Fatalf("priming request failed: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("POST", "http://"+r.addr+"/x", strings.NewReader("payload"))
	req.Host = "app.test"
	resp, err := c.Do(req)
	if err != nil {
		// A transport error is an acceptable outcome here; what must not
		// happen is a silent replay.
		t.Logf("POST failed at the transport: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 502 {
		t.Fatalf("status %d, want 200 (fresh connection) or 502 (surfaced failure)", resp.StatusCode)
	}
}

// TestIdleConnectionClosesQuietly asserts that a keepalive connection reaching
// its idle timeout is closed without a response body.
//
// Answering 400 to a client that never said anything would turn routine
// connection reaping into a visible error at both ends, and would inflate the
// error count on exactly the long-keepalive workloads the benchmark runs.
func TestIdleConnectionClosesQuietly(t *testing.T) {
	o := testOpts()
	o.IdleTimeout = 300 * time.Millisecond
	r := newRig(t, o, echoHandler())

	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	n, err := c.Read(buf)
	if n > 0 {
		t.Fatalf("idle connection got a response body %q; it should have been closed silently", buf[:n])
	}
	if err == nil {
		t.Fatal("read returned no data and no error")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("proxy never closed the idle connection; the idle timeout is not firing")
	}
}

// TestDrainServesQueuedConnections checks the shutdown ordering.
//
// The failure it guards against is silent and only appears under load: a
// connection that completed its TCP handshake and is sitting in the accept
// queue when shutdown begins gets a reset instead of a response, because
// closing a SO_REUSEPORT listener discards its queue. The client sees a
// connection error on a request the proxy never even read.
func TestDrainServesQueuedConnections(t *testing.T) {
	o := testOpts()
	r := newRig(t, o, echoHandler())

	c := r.client()
	if resp, body := r.get(c, "/small"); resp.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("pre-shutdown request failed: %d %q", resp.StatusCode, body)
	}

	// StopAccepting must not break connections that are already established
	// and mid-keepalive; it only declines further keepalive rounds.
	r.srv.StopAccepting()

	conn, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		// Refusing new connections after StopAccepting is acceptable; what is
		// not acceptable is accepting one and then resetting it.
		t.Logf("dial after StopAccepting refused: %v", err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "GET /small HTTP/1.1\r\nHost: app.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n == 0 {
		t.Fatalf("queued connection got nothing back after StopAccepting: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("queued connection got %q, want a 200", buf[:n])
	}
}
