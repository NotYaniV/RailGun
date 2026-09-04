package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSustainedLoad drives the proxy hard enough, for long enough, to expose
// the failures that a short functional test cannot.
//
// It is not a performance measurement and must not be read as one: it runs on
// a developer machine against a loopback origin, which is precisely the
// environment design spec section 4.2 says produces numbers that mean nothing.
// What it does check is that nothing accumulates. A proxy that leaks one file
// descriptor, one slab slot, or one goroutine per request passes every test
// above and dies in the first minute of a real run.
func TestSustainedLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained load test skipped in -short")
	}

	o := testOpts()
	o.MaxUpstreamConns = 128
	r := newRig(t, o, echoHandler())

	goroutinesBefore := runtime.NumGoroutine()

	const workers = 32
	const perWorker = 150

	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := r.client()
			// Mix body sizes so both sides of the splice threshold are
			// exercised, and mix in a chunked response so the scanner has to
			// resynchronize the connection repeatedly rather than once.
			paths := []string{"/small", "/bytes/1024", "/bytes/65536", "/chunked", "/bytes/16"}
			for j := 0; j < perWorker; j++ {
				p := paths[(id+j)%len(paths)]
				req, _ := http.NewRequest("GET", "http://"+r.addr+p, nil)
				req.Host = "app.test"
				resp, err := c.Do(req)
				if err != nil {
					failed.Add(1)
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					failed.Add(1)
					continue
				}
				_ = n
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := workers * perWorker
	if failed.Load() > 0 {
		t.Errorf("%d of %d requests failed", failed.Load(), total)
	}
	if ok.Load() != int64(total) {
		t.Errorf("completed %d of %d requests", ok.Load(), total)
	}

	t.Logf("%d requests in %v (%.0f req/s on loopback, NOT a benchmark figure)",
		total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds())

	// The slab must not be holding more connections than the pools could
	// plausibly have idle. A per-request leak shows up here as a number in the
	// hundreds rather than the tens.
	if inUse := r.srv.UpstreamConnsInUse(); inUse > workers*2 {
		t.Errorf("slab holds %d upstream connections after %d requests from %d workers; suspect a per-request leak",
			inUse, total, workers)
	}

	var dials, reuses, overflowed uint64
	for _, st := range r.srv.PoolStats() {
		dials += st.Dials
		reuses += st.Reuses
		overflowed += st.Overflowed
	}
	t.Logf("pool: %d dials, %d reuses, %d overflowed", dials, reuses, overflowed)
	if dials > uint64(total)/2 {
		t.Errorf("%d dials for %d requests; the pool is barely reusing anything", dials, total)
	}

	// Goroutines need a moment to unwind after the last response.
	deadline := time.Now().Add(3 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= goroutinesBefore+workers {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > goroutinesBefore+workers {
		t.Errorf("goroutines went from %d to %d across %d requests; suspect a leak",
			goroutinesBefore, after, total)
	}
}

// BenchmarkProxyThroughput exists to be run under a profiler, not to produce a
// headline. Loopback, a Go origin sharing the same cores, and no rig isolation
// mean the absolute number is meaningless; what it is good for is `-cpuprofile`
// showing where the proxy's own time goes.
func BenchmarkProxyThroughput(b *testing.B) {
	for _, size := range []int{1024, 65536} {
		b.Run(fmt.Sprintf("body=%d", size), func(b *testing.B) {
			t := &testing.T{}
			r := newRig(t, testOpts(), echoHandler())
			path := fmt.Sprintf("/bytes/%d", size)

			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				c := r.client()
				for pb.Next() {
					req, _ := http.NewRequest("GET", "http://"+r.addr+path, nil)
					req.Host = "app.test"
					resp, err := c.Do(req)
					if err != nil {
						b.Fatal(err)
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			})
		})
	}
}
