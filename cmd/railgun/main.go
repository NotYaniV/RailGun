// Command railgun is the Approach A reverse proxy.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/metrics"
	"github.com/NotYaniV/RailGun/internal/proxy"
	"github.com/NotYaniV/RailGun/internal/sysx"
)

func main() {
	var (
		addr        = flag.String("addr", ":8080", "listen address")
		cfgPath     = flag.String("config", "", "path to the route config JSON")
		adminAddr   = flag.String("admin", "127.0.0.1:9090", "admin/metrics listen address, empty to disable")
		listeners   = flag.Int("listeners", 0, "SO_REUSEPORT listeners, 0 means GOMAXPROCS")
		forceCopy   = flag.Bool("force-copy", false, "route response bodies through a userspace buffer, never splice (experiment E2 control arm)")
		spliceMin   = flag.Int64("splice-threshold", 0, "body size at or above which splice is used, 0 means the built-in default")
		gogc        = flag.Int("gogc", 400, "GOGC target")
		memLimit    = flag.String("memlimit", "", "GOMEMLIMIT, e.g. 512MiB; required for a benchmark run")
		instrument  = flag.Bool("instrument", true, "record latency histograms")
		sampleRate  = flag.Uint("sample-rate", 16, "record 1 in N requests when instrumentation is on")
		reapEvery   = flag.Duration("reap-interval", 10*time.Second, "idle upstream connection sweep interval")
		drainWindow = flag.Duration("drain", 5*time.Second, "shutdown drain window")
	)
	flag.Parse()

	if *cfgPath == "" {
		log.Fatal("railgun: -config is required")
	}
	snap, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("railgun: config: %v", err)
	}
	var store config.Store
	store.Store(snap)

	// GC tuning, per spec 5.5 and 2. GOGC buys reduced collection frequency
	// with resident memory, which is the declared sacrifice; GOMEMLIMIT is the
	// enforced ceiling on that sacrifice rather than a figure discovered after
	// the run. A benchmark run without a limit set is not a valid data point,
	// so the absence is warned about loudly rather than defaulted silently.
	debug.SetGCPercent(*gogc)
	if *memLimit != "" {
		n, err := parseBytes(*memLimit)
		if err != nil {
			log.Fatalf("railgun: -memlimit: %v", err)
		}
		debug.SetMemoryLimit(n)
	} else {
		log.Print("railgun: warning: GOMEMLIMIT unset; spec section 2 requires it be set to the RSS budget for any published run")
	}

	metrics.SetEnabled(*instrument)
	metrics.SetSampleRate(uint32(*sampleRate))

	opts := proxy.DefaultOptions()
	opts.ForceCopy = *forceCopy
	if *spliceMin > 0 {
		opts.SpliceThreshold = *spliceMin
	}

	n := *listeners
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	// n listeners in one SO_REUSEPORT group: the kernel's 4-tuple hash
	// distributes accepts with no userspace lock, which is how spec 5.1
	// sidesteps NGINX's accept_mutex question rather than answering it.
	lns, err := sysx.Listen(*addr, n)
	if err != nil {
		log.Fatalf("railgun: listen: %v", err)
	}

	srv := proxy.New(&store, opts)

	// The run's configuration is printed as one line so it lands in the result
	// set. Spec 5.8 requires a run to report whether instrumentation was on,
	// because a peak-RPS number taken with it on is not comparable to one
	// taken with it off.
	log.Printf("railgun: addr=%s listeners=%d gomaxprocs=%d gogc=%d memlimit=%s instrument=%v sample=1/%d force-copy=%v splice-threshold=%d",
		*addr, len(lns), runtime.GOMAXPROCS(0), *gogc, orNone(*memLimit), *instrument, *sampleRate, *forceCopy, opts.SpliceThreshold)

	if *adminAddr != "" {
		go serveAdmin(*adminAddr, srv)
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(*reapEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				srv.ReapIdle()
			case <-stop:
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Print("railgun: draining")
		// Order matters: stop accepting before touching the listeners. The
		// drain polls each accept queue with a short deadline, which wakes any
		// accept goroutine parked in AcceptTCP with a timeout; unless the
		// server already knows it is shutting down, it reads that as a real
		// failure and tears down the accept loop in the middle of the drain.
		srv.StopAccepting()
		// Spec 5.7 describes removing listeners from the reuseport group
		// before closing them. Linux has no such operation, and there is
		// nothing to hand off to in a single-process design anyway. What is
		// real is the loss: closing a reuseport listener discards connections
		// already queued on it, and those clients completed a handshake and
		// would get a reset for a request never read. So the drain accepts the
		// queues and serves what it finds, then closes.
		drained, err := sysx.CloseAfterDrain(lns, *drainWindow, srv.ServeConn)
		if err != nil {
			log.Printf("railgun: drain: %v", err)
		}
		log.Printf("railgun: drained %d queued connections", drained)

		deadline := make(chan struct{})
		go func() { time.Sleep(*drainWindow); close(deadline) }()
		srv.Shutdown(deadline)
		close(stop)
	}()

	if err := srv.Serve(lns); err != nil {
		log.Printf("railgun: serve: %v", err)
	}
	log.Print("railgun: stopped")
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// serveAdmin exposes metrics and pool statistics.
//
// It runs on its own listener and uses net/http deliberately: it is off the
// request path entirely, so the allocation behaviour that disqualifies
// net/http for proxying is irrelevant here, and hand-rolling a second HTTP
// server to serve a metrics page would be work spent for no measurable gain.
func serveAdmin(addr string, srv *proxy.Server) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		snap := metrics.Scrape()
		if err := snap.WriteText(w); err != nil {
			return
		}
		for a, st := range srv.PoolStats() {
			fmt.Fprintf(w, "railgun_pool_idle{upstream=%q} %d\n", a, st.Idle)
			fmt.Fprintf(w, "railgun_pool_reuses{upstream=%q} %d\n", a, st.Reuses)
			fmt.Fprintf(w, "railgun_pool_dials{upstream=%q} %d\n", a, st.Dials)
			fmt.Fprintf(w, "railgun_pool_dial_failures{upstream=%q} %d\n", a, st.DialFailures)
			fmt.Fprintf(w, "railgun_pool_stale_evicted{upstream=%q} %d\n", a, st.StaleEvicted)
			fmt.Fprintf(w, "railgun_pool_overflowed{upstream=%q} %d\n", a, st.Overflowed)
		}
		fmt.Fprintf(w, "railgun_upstream_conns_in_use %d\n", srv.UpstreamConnsInUse())
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("railgun: admin: %v", err)
	}
}

// parseBytes accepts a plain byte count or a suffixed size such as 512MiB.
func parseBytes(s string) (int64, error) {
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	} {
		if len(s) > len(suf.s) && s[len(s)-len(suf.s):] == suf.s {
			mult = suf.m
			s = s[:len(s)-len(suf.s)]
			break
		}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("not a byte size: %q", s)
	}
	return n * mult, nil
}
