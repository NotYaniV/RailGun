package upstream

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Experiment E1 (spec §5.4). Variant A is a single sync.Mutex per upstream and
// every request performs one Acquire and one Release, so the spec's own
// arithmetic says a global mutex is a bottleneck by construction rather than by
// hypothesis. These benchmarks turn that into a number.
//
// The question is not "is there a mutex" — there is. It is: at what request
// rate does this mutex stop being free? Run E1 with:
//
//	make e1
//
// which captures -mutexprofile and -blockprofile and prints their top frames.
// Variant B is built only if those profiles show the pool mutex as a
// significant contention site (spec §5.4). If they do not, the null result is
// the finding and B is never built.
//
// All I/O is faked, so what is measured is pool bookkeeping and lock
// contention, nothing else.

func benchPool(b *testing.B, maxIdle int) *Pool {
	b.Helper()
	var next atomic.Int64
	next.Store(1)
	return New("bench", Config{
		Dial:  func(string) (int, error) { return int(next.Add(1)), nil },
		Close: func(int) error { return nil },
		// Large enough that a steady state of one connection per goroutine
		// never overflows: an overflow would close and redial, which measures
		// the dial path instead of the lock.
		MaxIdle: maxIdle,
		// Far longer than any run, so nothing expires mid-benchmark.
		MaxIdleTime: time.Hour,
	})
}

// BenchmarkAcquireReleaseUncontended is the floor: one goroutine, warm pool, no
// contention. This is the per-request cost the pool adds when nothing is
// competing for it.
func BenchmarkAcquireReleaseUncontended(b *testing.B) {
	p := benchPool(b, 1024)
	tok, _, err := p.Acquire()
	if err != nil {
		b.Fatal(err)
	}
	p.Release(tok)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tok, _, err := p.Acquire()
		if err != nil {
			b.Fatal(err)
		}
		p.Release(tok)
	}
}

// BenchmarkAcquireReleaseCoarseClock is the same as Uncontended but with a
// clock that costs nothing, isolating what time.Now contributes.
//
// The pool reads the clock twice per request — once in Acquire to age out
// expired entries, once in Release to stamp the returned one. Config.Now exists
// so the proxy can hand the pool a coarse cached time updated once per event
// loop pass, which is exactly what NGINX does with its cached time and for the
// same reason. This benchmark says whether that is worth doing.
func BenchmarkAcquireReleaseCoarseClock(b *testing.B) {
	fixed := time.Unix(1<<30, 0)
	var next atomic.Int64
	p := New("bench", Config{
		Dial:        func(string) (int, error) { return int(next.Add(1)), nil },
		Close:       func(int) error { return nil },
		MaxIdle:     1024,
		MaxIdleTime: time.Hour,
		Now:         func() time.Time { return fixed },
	})
	tok, _, err := p.Acquire()
	if err != nil {
		b.Fatal(err)
	}
	p.Release(tok)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tok, _, err := p.Acquire()
		if err != nil {
			b.Fatal(err)
		}
		p.Release(tok)
	}
}

// BenchmarkAcquireReleaseContended runs GOMAXPROCS goroutines through the same
// pool, which is exactly variant A's worst case.
//
// hold is the work done between Acquire and Release. It is the parameter that
// decides whether this experiment means anything: at hold=0 every goroutine is
// inside the critical section essentially all the time, which is the true worst
// case but is not a real proxy. A real request holds its upstream connection
// for the duration of an origin round trip, so the sub-benchmarks span from the
// pathological case up to a realistic one, and the honest verdict is read off
// the row that matches the measured origin latency.
func BenchmarkAcquireReleaseContended(b *testing.B) {
	for _, hold := range []time.Duration{0, 100 * time.Nanosecond, time.Microsecond, 10 * time.Microsecond} {
		b.Run(holdName(hold), func(b *testing.B) {
			p := benchPool(b, 4096)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					tok, _, err := p.Acquire()
					if err != nil {
						b.Error(err)
						return
					}
					spin(hold)
					p.Release(tok)
				}
			})
		})
	}
}

// BenchmarkMutexOnlyContended is the control. It is the same parallelism over a
// bare sync.Mutex with an empty critical section, so the gap between it and
// BenchmarkAcquireReleaseContended/hold=0 is the pool's own bookkeeping and the
// rest is the lock itself.
//
// Without this control a slow contended number is unattributable: it could be
// the mutex, or it could be the slice manipulation and the counters. Variant B
// only helps with the former.
func BenchmarkMutexOnlyContended(b *testing.B) {
	var mu sync.Mutex
	var n int

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			n++
			mu.Unlock()
			mu.Lock()
			n++
			mu.Unlock()
		}
	})
	_ = n
}

// BenchmarkColdDial measures the path taken when the pool is empty. It is not
// part of the E1 verdict — it exists so a contended number can be checked
// against it, since a benchmark that has accidentally fallen into the dial path
// is measuring something else entirely.
func BenchmarkColdDial(b *testing.B) {
	p := benchPool(b, 0) // MaxIdle 0 becomes the default 32; Discard keeps it empty

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tok, reused, err := p.Acquire()
		if err != nil {
			b.Fatal(err)
		}
		if reused {
			b.Fatal("pool was supposed to be cold")
		}
		p.Discard(tok)
	}
}

// spin burns approximately d without yielding, standing in for the CPU work a
// request does while holding an upstream connection. time.Sleep would park the
// goroutine and hand the lock to someone else, which is the opposite of what
// this is trying to reproduce.
func spin(d time.Duration) {
	if d == 0 {
		return
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
	}
}

func holdName(d time.Duration) string {
	if d == 0 {
		return "hold=0"
	}
	return "hold=" + d.String()
}
