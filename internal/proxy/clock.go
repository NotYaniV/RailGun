package proxy

import (
	"sync/atomic"
	"time"
)

// coarseClock is a cached wall clock updated on a ticker, read with a single
// atomic load.
//
// It exists because time.Now is expensive at the request rates this project
// targets and is called more often than it looks. Measured on darwin/arm64,
// two time.Now calls were 83% of the upstream pool's entire per-request cost:
// 71.4ns/op with a real clock against 11.8ns/op with a constant one, for the
// same lock, slice manipulation and counter updates. Linux's vDSO
// clock_gettime is cheaper at roughly 20-25ns, so the ratio narrows on the
// rig, but the clock still dominates a critical section whose real work is
// single-digit nanoseconds.
//
// NGINX caches its clock for exactly this reason, updating it once per event
// loop iteration rather than per use. This is the same idea with a timer
// instead of an event loop, because Go's netpoll gives no per-iteration hook
// to hang the update on.
//
// The precision given up is real and bounded: every reader sees a timestamp up
// to one tick stale. Everything here consumes it for durations measured in
// tens of milliseconds upward — idle-connection expiry, dial backoff, request
// deadlines — so a millisecond of skew changes no decision. It is emphatically
// NOT suitable for latency measurement, and the metrics package does not use
// it: spec 5.8 samples latency precisely so that a real time.Now can be
// afforded on the requests that are measured.
type coarseClock struct {
	ns   atomic.Int64
	stop chan struct{}
}

// newCoarseClock starts a clock updated every interval. Callers must Close it.
func newCoarseClock(interval time.Duration) *coarseClock {
	c := &coarseClock{stop: make(chan struct{})}
	c.ns.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case now := <-t.C:
				c.ns.Store(now.UnixNano())
			case <-c.stop:
				return
			}
		}
	}()
	return c
}

// Now returns the cached time. It is an atomic load and a struct construction;
// no allocation and no syscall.
//
// The returned time carries no monotonic reading, so subtracting two of these
// is subject to wall-clock adjustment. That is acceptable for the timeout
// arithmetic it feeds and unacceptable for anything measuring elapsed time,
// which is why nothing here does.
func (c *coarseClock) Now() time.Time { return time.Unix(0, c.ns.Load()) }

// Close stops the updating goroutine.
func (c *coarseClock) Close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}
