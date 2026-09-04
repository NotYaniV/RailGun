// Package metrics provides per-request instrumentation for Railgun:
// a per-P sharded, lock-free latency histogram plus a handful of plain
// counters, designed so the record path costs nanoseconds, not
// microseconds. See design spec §5.8 "Instrumentation" and §4.5
// "Metrics recorded for every run".
//
// Design summary:
//
//   - The record path never takes a lock and never contends an atomic
//     across cores in the common case: each logical CPU (P) owns its
//     own cache-line-padded shard, so writers on different cores touch
//     different cache lines.
//   - Latency instrumentation is sampled and can be switched off at
//     runtime. When off, the fast check (Enabled/ShouldSample) costs a
//     single atomic load and a predictable branch — no time.Now() is
//     ever taken on the disabled path, because that decision belongs
//     to the caller: check ShouldSample() first, and only take a
//     timestamp if it returns true.
//   - Plain counters (requests, bytes copied, dials, pool hits/misses,
//     errors) are always on — they involve no timestamps and cost only
//     a couple of nanoseconds, so peak-RPS runs keep them even with
//     latency instrumentation off.
//   - Scrape (percentiles + counters) merges every shard. It allocates
//     and may take its time: it runs once per collection, not once per
//     request.
package metrics

import (
	"fmt"
	"io"
	"math"
	"math/bits"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe" // required for go:linkname, and used directly for Sizeof
)

// ---------------------------------------------------------------------
// P-pinning.
//
// There is no exported API to ask "which logical P am I running on
// right now". sync.Pool needs exactly that to pick a per-P pool slot,
// and the runtime exposes it to the sync package via two linknamed
// functions. We pull the same two symbols.
//
// This is the same mechanism sync.Pool relies on internally, so it is
// about as stable as a go:linkname trick can be: if the runtime ever
// removed sync.runtime_procPin/runtime_procUnpin, sync.Pool itself
// would stop compiling. Measured cost on darwin/arm64 (Apple M3):
// ~3.5ns for a Pin+Unpin pair, confirmed building for both
// darwin/arm64 and linux/amd64. Budget is ~25ns/op for the whole
// record path, so this leaves ample room.
//
// procPin disables preemption on the calling goroutine until
// procUnpin, so the shard index read here stays valid for the short
// window between Pin and Unpin.
//
//go:linkname procPin sync.runtime_procPin
func procPin() int

//go:linkname procUnpin sync.runtime_procUnpin
func procUnpin()

// ---------------------------------------------------------------------
// Latency histogram: fixed, log-ish buckets from 1µs to ~16.8s.

const (
	histBaseNS        = int64(1000)           // 1µs, the first bucket's lower bound
	histFiniteBuckets = 24                    // 1µs * 2^24 ≈ 16.78s
	histBuckets       = histFiniteBuckets + 1 // +1 overflow ("+Inf") bucket
	cacheLine         = 64
)

// bucketIndex returns the histogram bucket for duration d. Buckets
// double: bucket i covers [1µs<<i, 1µs<<(i+1)). Anything below 1µs
// falls into bucket 0; anything at or above the top finite boundary
// falls into the overflow bucket (histFiniteBuckets).
func bucketIndex(d time.Duration) int {
	ns := int64(d)
	if ns < histBaseNS {
		return 0
	}
	n := uint64(ns) / uint64(histBaseNS)
	idx := bits.Len64(n) - 1 // floor(log2(n)), n >= 1
	if idx >= histFiniteBuckets {
		return histFiniteBuckets
	}
	return idx
}

// bucketUpperNS returns the (exclusive) upper bound of bucket i in
// nanoseconds, used as the percentile estimate for that bucket.
func bucketUpperNS(i int) int64 {
	if i >= histFiniteBuckets {
		return histBaseNS << uint(histFiniteBuckets)
	}
	return histBaseNS << uint(i+1)
}

// histShardData is the per-P histogram state before cache-line padding.
type histShardData struct {
	buckets [histBuckets]atomic.Uint64
	max     atomic.Int64  // exact max latency (ns) seen on this shard
	sampleN atomic.Uint64 // 1-in-N sampling counter, owned by this shard
}

const histShardPad = (cacheLine - int(unsafe.Sizeof(histShardData{})%cacheLine)) % cacheLine

// histShard is histShardData padded to a full cache line so adjacent
// shards never share a line.
type histShard struct {
	histShardData
	_ [histShardPad]byte
}

// ---------------------------------------------------------------------
// Plain counters.

// ErrClass classifies a recorded error for the errors-by-class counter.
type ErrClass uint8

const (
	ErrUpstreamDial ErrClass = iota
	ErrUpstreamTimeout
	ErrUpstreamReset
	ErrClientTimeout
	ErrBadGateway
	ErrOther
	numErrClasses
)

func (c ErrClass) String() string {
	switch c {
	case ErrUpstreamDial:
		return "upstream_dial"
	case ErrUpstreamTimeout:
		return "upstream_timeout"
	case ErrUpstreamReset:
		return "upstream_reset"
	case ErrClientTimeout:
		return "client_timeout"
	case ErrBadGateway:
		return "bad_gateway"
	default:
		return "other"
	}
}

// counterShardData is the per-P counter state before cache-line padding.
type counterShardData struct {
	requests      atomic.Uint64
	bytesCopied   atomic.Uint64
	upstreamDials atomic.Uint64
	poolHits      atomic.Uint64
	poolMisses    atomic.Uint64
	errors        [numErrClasses]atomic.Uint64
}

const counterShardPad = (cacheLine - int(unsafe.Sizeof(counterShardData{})%cacheLine)) % cacheLine

// counterShard is counterShardData padded to a full cache line.
type counterShard struct {
	counterShardData
	_ [counterShardPad]byte
}

// ---------------------------------------------------------------------
// Global state.

var (
	enabled    atomic.Bool
	sampleRate atomic.Uint32 // 1-in-N; <=1 means "sample every call"

	histShards    []histShard
	counterShards []counterShard
)

func init() {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	histShards = make([]histShard, n)
	counterShards = make([]counterShard, n)
	sampleRate.Store(1)
}

// shardIndex clamps a procPin() result into [0, n). Under normal
// operation p is already < n (shards are sized from GOMAXPROCS at
// init); the modulo only matters if GOMAXPROCS grows after init,
// which a running proxy does not do in practice.
//
// ponytail: no dynamic shard-array resize on GOMAXPROCS change —
// clamp instead. Revisit only if the binary ever calls
// debug.SetGOMAXPROCS after startup.
func shardIndex(p, n int) int {
	if p >= n {
		return p % n
	}
	return p
}

// ---------------------------------------------------------------------
// Latency instrumentation: toggle, sampling, recording.

// Enabled reports whether latency instrumentation is currently active.
// Cost when off: one atomic load. Callers must check this (directly,
// or via ShouldSample) BEFORE taking a time.Now() timestamp so that no
// timestamp is ever taken while instrumentation is off.
func Enabled() bool {
	return enabled.Load()
}

// SetEnabled turns latency instrumentation on or off at runtime.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// SetSampleRate sets N in "1-in-N" sampling. Values <= 1 sample every
// call while instrumentation is enabled.
func SetSampleRate(n uint32) {
	sampleRate.Store(n)
}

// SampleRate returns the current 1-in-N sampling rate.
func SampleRate() uint32 {
	return sampleRate.Load()
}

// ShouldSample reports whether the caller should time and record this
// particular request: it combines the on/off switch with 1-in-N
// sampling. Call this BEFORE taking a time.Now() timestamp; only take
// one, and only call RecordLatency, if this returns true.
//
// Cost when disabled: a single predictable-branch atomic load and
// return — no time.Now(), no shard access.
func ShouldSample() bool {
	if !enabled.Load() {
		return false
	}
	rate := sampleRate.Load()
	if rate <= 1 {
		return true
	}
	p := procPin()
	s := &histShards[shardIndex(p, len(histShards))]
	n := s.sampleN.Add(1)
	procUnpin()
	return n%uint64(rate) == 0
}

// RecordLatency records one latency sample. Call only for a request
// for which ShouldSample returned true. The record path is: compute
// the bucket index, then bump that bucket and the shard's running max
// — no lock, and the atomics touch only this shard's own cache line
// under normal (uncontended) operation.
func RecordLatency(d time.Duration) {
	idx := bucketIndex(d)
	p := procPin()
	s := &histShards[shardIndex(p, len(histShards))]
	s.buckets[idx].Add(1)
	ns := int64(d)
	for {
		cur := s.max.Load()
		if ns <= cur {
			break
		}
		if s.max.CompareAndSwap(cur, ns) {
			break
		}
	}
	procUnpin()
}

// ---------------------------------------------------------------------
// Plain counters: always on, no timestamps involved.

// IncRequest counts one request. Always active regardless of the
// instrumentation toggle, so RPS can still be computed on peak-RPS
// runs with latency instrumentation off.
func IncRequest() {
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].requests.Add(1)
	procUnpin()
}

// AddBytesCopied adds n to the userspace-copied-bytes counter (the
// splice-vs-copy experiment E2 reads this; see spec §4.5 and §5.5).
func AddBytesCopied(n uint64) {
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].bytesCopied.Add(n)
	procUnpin()
}

// IncUpstreamDial counts one new upstream connection dial.
func IncUpstreamDial() {
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].upstreamDials.Add(1)
	procUnpin()
}

// IncPoolHit counts one upstream connection reused from the pool.
func IncPoolHit() {
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].poolHits.Add(1)
	procUnpin()
}

// IncPoolMiss counts one upstream pool lookup that found nothing
// reusable.
func IncPoolMiss() {
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].poolMisses.Add(1)
	procUnpin()
}

// IncError counts one error of the given class. Unknown class values
// are folded into ErrOther.
func IncError(c ErrClass) {
	if c >= numErrClasses {
		c = ErrOther
	}
	p := procPin()
	counterShards[shardIndex(p, len(counterShards))].errors[c].Add(1)
	procUnpin()
}

// ---------------------------------------------------------------------
// Scrape: merge shards, compute percentiles. Runs once per collection,
// not once per request — free to allocate.

// Snapshot is a point-in-time merge of every shard.
type Snapshot struct {
	InstrumentationEnabled bool
	SampleRate             uint32

	Requests      uint64
	BytesCopied   uint64
	UpstreamDials uint64
	PoolHits      uint64
	PoolMisses    uint64
	Errors        [numErrClasses]uint64

	Count               uint64 // total latency samples recorded
	P50, P90, P99, P999 time.Duration
	Max                 time.Duration
}

// Scrape merges all per-P shards into a single Snapshot.
func Scrape() Snapshot {
	var merged [histBuckets]uint64
	var total uint64
	var maxNS int64
	for i := range histShards {
		s := &histShards[i]
		for b := 0; b < histBuckets; b++ {
			v := s.buckets[b].Load()
			merged[b] += v
			total += v
		}
		if m := s.max.Load(); m > maxNS {
			maxNS = m
		}
	}

	var snap Snapshot
	for i := range counterShards {
		c := &counterShards[i]
		snap.Requests += c.requests.Load()
		snap.BytesCopied += c.bytesCopied.Load()
		snap.UpstreamDials += c.upstreamDials.Load()
		snap.PoolHits += c.poolHits.Load()
		snap.PoolMisses += c.poolMisses.Load()
		for e := 0; e < int(numErrClasses); e++ {
			snap.Errors[e] += c.errors[e].Load()
		}
	}

	snap.InstrumentationEnabled = enabled.Load()
	snap.SampleRate = sampleRate.Load()
	snap.Count = total
	snap.P50 = percentile(merged, total, 0.50)
	snap.P90 = percentile(merged, total, 0.90)
	snap.P99 = percentile(merged, total, 0.99)
	snap.P999 = percentile(merged, total, 0.999)
	snap.Max = time.Duration(maxNS)
	return snap
}

// percentile estimates the p-th percentile (p in (0,1]) from merged
// bucket counts, returning the upper bound of the first bucket whose
// cumulative count reaches the target rank.
func percentile(buckets [histBuckets]uint64, total uint64, p float64) time.Duration {
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(float64(total) * p))
	if target < 1 {
		target = 1
	}
	var cum uint64
	for i, c := range buckets {
		cum += c
		if cum >= target {
			return time.Duration(bucketUpperNS(i))
		}
	}
	return time.Duration(bucketUpperNS(histBuckets - 1))
}

// WriteText writes the snapshot in a simple Prometheus-style text
// exposition format (no third-party dependency).
func (s Snapshot) WriteText(w io.Writer) error {
	enabledVal := 0
	if s.InstrumentationEnabled {
		enabledVal = 1
	}
	lines := []struct {
		name string
		val  interface{}
	}{
		{"railgun_instrumentation_enabled", enabledVal},
		{"railgun_sample_rate", s.SampleRate},
		{"railgun_requests_total", s.Requests},
		{"railgun_bytes_copied_total", s.BytesCopied},
		{"railgun_upstream_dials_total", s.UpstreamDials},
		{"railgun_pool_hits_total", s.PoolHits},
		{"railgun_pool_misses_total", s.PoolMisses},
		{"railgun_latency_samples_total", s.Count},
	}
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%s %v\n", l.name, l.val); err != nil {
			return err
		}
	}
	for e := ErrClass(0); e < numErrClasses; e++ {
		if _, err := fmt.Fprintf(w, "railgun_errors_total{class=%q} %d\n", e.String(), s.Errors[e]); err != nil {
			return err
		}
	}
	quantiles := []struct {
		q string
		d time.Duration
	}{
		{"0.5", s.P50},
		{"0.9", s.P90},
		{"0.99", s.P99},
		{"0.999", s.P999},
	}
	for _, q := range quantiles {
		if _, err := fmt.Fprintf(w, "railgun_latency_seconds{quantile=%q} %g\n", q.q, q.d.Seconds()); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "railgun_latency_seconds_max %g\n", s.Max.Seconds()); err != nil {
		return err
	}
	return nil
}
