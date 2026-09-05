package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/proxy"
	"github.com/NotYaniV/RailGun/internal/sysx"
)

// Serve spawns one accept goroutine per listener and sysx.Listen hands it a
// SO_REUSEPORT group. Neither side had a test crossing to the other.
//
// The failure this catches is quiet by construction. If Serve accepted on only
// the first listener — a loop variable captured wrong, a range that stopped
// early — then on Linux the kernel would still hash connections across all
// members of the group and roughly (n-1)/n of them would sit in accept queues
// nobody reads. They do not fail; they hang until a timeout, which under a
// benchmark reads as tail latency rather than as a bug. The RPS number would
// still be produced, and it would be wrong in a direction nobody would question.
//
// So the assertion is not about distribution, which is the kernel's business
// and is checked in internal/sysx. It is that EVERY connection to the group is
// served, which is only true if every member has a reader.
func TestServeAcceptsOnEveryListenerInTheGroup(t *testing.T) {
	const listeners = 8
	const clients = 64

	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })

	snap, err := config.NewSnapshot(1,
		[]config.Route{{Host: "app.test", Upstream: "app"}},
		[]config.Upstream{{Name: "app", Addrs: []string{oln.Addr().String()}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var store config.Store
	store.Store(snap)

	lns, err := sysx.Listen("127.0.0.1:0", listeners)
	if err != nil {
		t.Fatal(err)
	}
	if len(lns) != listeners {
		t.Fatalf("sysx.Listen returned %d listeners, want %d", len(lns), listeners)
	}
	addr := lns[0].Addr().String()

	srv := proxy.New(&store, testOpts())
	go srv.Serve(lns)
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		for _, l := range lns {
			l.Close()
		}
		srv.Shutdown(done)
	})

	// Every connection is a distinct 4-tuple, so on Linux they hash across the
	// whole group. All of them must be answered.
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			// Short relative to the test timeout: a connection accepted by
			// nobody stalls here rather than failing, which is the whole shape
			// of the bug being guarded against.
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: app.test\r\n\r\n"); err != nil {
				errs <- err
				return
			}
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				errs <- err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)

	failed := 0
	var first error
	for err := range errs {
		failed++
		if first == nil {
			first = err
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d connections to the reuseport group were never served "+
			"(first: %v); at least one listener in the group has no accept "+
			"goroutine reading it", failed, clients, first)
	}
}
