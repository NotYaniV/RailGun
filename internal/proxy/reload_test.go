package proxy_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/proxy"
)

// namedOrigin serves a fixed body so a test can tell which upstream answered,
// after an optional delay so a request can be caught mid-flight.
func namedOrigin(t *testing.T, name string, delay time.Duration) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" && delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(name)))
		io.WriteString(w, name)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln
}

func snapshotTo(t *testing.T, version uint64, addr string) *config.Snapshot {
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

// TestConfigSwapDoesNotMoveAnInFlightRequest checks the claim design spec 5.6
// makes for the atomic-pointer configuration: a reload swaps the pointer, and
// in-flight requests finish against the snapshot they started with while new
// requests see the new one.
//
// Nothing checked it before. The property holds by construction — exchange
// loads the pointer once and uses that snapshot for the whole request — but
// "holds by construction" is what was true of several defects already found in
// this package, and it is exactly the kind of cross-component claim that a unit
// test of either side cannot reach: it needs a live request in flight, a real
// reload, and a way to tell which upstream actually answered.
func TestConfigSwapDoesNotMoveAnInFlightRequest(t *testing.T) {
	oldOrigin := namedOrigin(t, "OLD", 400*time.Millisecond)
	newOrigin := namedOrigin(t, "NEW", 0)

	var store config.Store
	store.Store(snapshotTo(t, 1, oldOrigin.Addr().String()))

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(&store, testOpts())
	go srv.Serve([]*net.TCPListener{pln.(*net.TCPListener)})
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		pln.Close()
		srv.Shutdown(done)
	})
	addr := pln.Addr().String()

	get := func(path string) string {
		req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		req.Host = "app.test"
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	inflight := make(chan string, 1)
	go func() { inflight <- get("/slow") }()

	// Let the request reach the old upstream, then reload underneath it.
	time.Sleep(120 * time.Millisecond)
	store.Store(snapshotTo(t, 2, newOrigin.Addr().String()))

	// A request started AFTER the swap must see the new upstream, and must not
	// have to wait for the in-flight one.
	if got := get("/fast"); got != "NEW" {
		t.Errorf("request after reload was answered by %q, want NEW; the new snapshot is not being picked up", got)
	}

	if got := <-inflight; got != "OLD" {
		t.Errorf("in-flight request was answered by %q, want OLD; the reload moved a request that had already been routed", got)
	}
}

// TestConfigSwapKeepsTheUpstreamPool checks the other half of the 5.6 claim,
// and the one that is actually a feature rather than an absence of a bug.
//
// A real NGINX reload drops the upstream keepalive pool: workers are replaced
// and their connections go with them. Railgun swaps a pointer, so pools live in
// the Server rather than in the configuration and survive untouched. If a
// reload ever started costing a pool, this catches it — and it is worth
// catching, because the cost would be invisible except as a latency spike right
// after every deploy.
func TestConfigSwapKeepsTheUpstreamPool(t *testing.T) {
	origin := namedOrigin(t, "SAME", 0)

	var store config.Store
	store.Store(snapshotTo(t, 1, origin.Addr().String()))

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(&store, testOpts())
	go srv.Serve([]*net.TCPListener{pln.(*net.TCPListener)})
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		pln.Close()
		srv.Shutdown(done)
	})
	addr := pln.Addr().String()

	c := &http.Client{Timeout: 10 * time.Second}
	do := func() {
		req, _ := http.NewRequest("GET", "http://"+addr+"/x", nil)
		req.Host = "app.test"
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	for i := 0; i < 5; i++ {
		do()
	}
	var dialsBefore uint64
	for _, st := range srv.PoolStats() {
		dialsBefore += st.Dials
	}

	// Reload to an identical-but-new snapshot, as a config file rewrite would.
	store.Store(snapshotTo(t, 2, origin.Addr().String()))

	for i := 0; i < 5; i++ {
		do()
	}
	var dialsAfter uint64
	for _, st := range srv.PoolStats() {
		dialsAfter += st.Dials
	}

	if dialsAfter > dialsBefore {
		t.Errorf("reload cost %d new upstream dials (%d -> %d); the keepalive pool did not survive the swap, which is the one thing this design has over an NGINX reload",
			dialsAfter-dialsBefore, dialsBefore, dialsAfter)
	}
	t.Logf("dials before reload %d, after %d", dialsBefore, dialsAfter)
}
