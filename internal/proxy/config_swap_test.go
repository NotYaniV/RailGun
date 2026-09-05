package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/proxy"
)

// namedOrigin answers every request with a fixed body. gate, when non-nil, is
// waited on before the response is written, so a test can hold an exchange open
// across a configuration swap.
func newNamedOrigin(t *testing.T, body string, gate <-chan struct{}) net.Listener {
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
					if gate != nil {
						<-gate
					}
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: "+
						itoa(len(body))+"\r\nConnection: keep-alive\r\n\r\n"+body)
				}
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// newRigOverStore is newRigTo with the config store handed in, so a test can
// swap configuration underneath a running proxy.
func newRigOverStore(t *testing.T, opts proxy.Options, store *config.Store) *rig {
	t.Helper()
	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(store, opts)
	go srv.Serve([]*net.TCPListener{pln.(*net.TCPListener)})
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		pln.Close()
		srv.Shutdown(done)
	})
	return &rig{t: t, srv: srv, addr: pln.Addr().String()}
}

func snapshotRouting(t *testing.T, version uint64, addr string) *config.Snapshot {
	t.Helper()
	snap, err := config.NewSnapshot(version,
		[]config.Route{{Host: "app.test", Upstream: "app"}},
		[]config.Upstream{{Name: "app", Addrs: []string{addr}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Spec §5.6 claims an in-flight request finishes against the snapshot it
// started with, and that the next request sees the new one. Nothing checked it.
//
// This is a boundary test rather than a unit test: config's atomic swap is
// obviously correct in isolation, and so is the proxy's single Load per
// exchange. What is only true at the seam is that the exchange holds its
// snapshot for the WHOLE exchange — a second Load anywhere after routing, or a
// snapshot captured per connection instead of per exchange, would break one
// half of the claim while leaving the other looking right.
func TestConfigSwapDoesNotDisturbAnInFlightExchange(t *testing.T) {
	release := make(chan struct{})
	originA := newNamedOrigin(t, "AAA", release)
	originB := newNamedOrigin(t, "BBB", nil)

	var store config.Store
	store.Store(snapshotRouting(t, 1, originA.Addr().String()))
	r := newRigOverStore(t, testOpts(), &store)

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	req := "GET /x HTTP/1.1\r\nHost: app.test\r\n\r\n"

	// Request one is in flight and parked inside origin A.
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	// Give the proxy time to route and reach the origin before swapping.
	time.Sleep(100 * time.Millisecond)

	// Swap the route out from under it.
	store.Store(snapshotRouting(t, 2, originB.Addr().String()))
	close(release)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("in-flight response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "AAA" {
		t.Fatalf("in-flight request answered %q; it must finish against the "+
			"snapshot it started with, not the one that replaced it", body)
	}

	// The NEXT request on the same keepalive connection must see the new
	// snapshot. A snapshot captured per connection rather than per exchange
	// would still answer AAA here.
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("post-swap response: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != "BBB" {
		t.Fatalf("request after the swap answered %q, want BBB — the new "+
			"snapshot is not being picked up per exchange", body2)
	}
}

// A route removed by a reload must stop resolving, and must do so without
// disturbing whatever is already in flight against it.
func TestConfigSwapRemovingARouteFailsLater(t *testing.T) {
	release := make(chan struct{})
	originA := newNamedOrigin(t, "AAA", release)

	var store config.Store
	store.Store(snapshotRouting(t, 1, originA.Addr().String()))
	r := newRigOverStore(t, testOpts(), &store)

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	req := "GET /x HTTP/1.1\r\nHost: app.test\r\n\r\n"

	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	// Reload to a configuration with no route for app.test at all.
	empty, err := config.NewSnapshot(2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.Store(empty)
	close(release)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("in-flight response after the route was removed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "AAA" {
		t.Fatalf("in-flight request answered %q after its route was removed; "+
			"it must still finish against the snapshot it started with", body)
	}
}
