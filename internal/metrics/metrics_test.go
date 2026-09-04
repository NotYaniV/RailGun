package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// resetForTest zeroes all shard state and restores default toggles.
// White-box: lives in the test file only, never in production code.
func resetForTest() {
	for i := range histShards {
		s := &histShards[i]
		for b := range s.buckets {
			s.buckets[b].Store(0)
		}
		s.max.Store(0)
		s.sampleN.Store(0)
	}
	for i := range counterShards {
		c := &counterShards[i]
		c.requests.Store(0)
		c.bytesCopied.Store(0)
		c.upstreamDials.Store(0)
		c.poolHits.Store(0)
		c.poolMisses.Store(0)
		for e := range c.errors {
			c.errors[e].Store(0)
		}
	}
	enabled.Store(false)
	sampleRate.Store(1)
}

func TestShardSizeIsCacheLineMultiple(t *testing.T) {
	hs := unsafe.Sizeof(histShard{})
	cs := unsafe.Sizeof(counterShard{})
	t.Logf("histShard size=%d counterShard size=%d cacheLine=%d", hs, cs, cacheLine)
	if hs%cacheLine != 0 {
		t.Errorf("unsafe.Sizeof(histShard{}) = %d, not a multiple of %d", hs, cacheLine)
	}
	if cs%cacheLine != 0 {
		t.Errorf("unsafe.Sizeof(counterShard{}) = %d, not a multiple of %d", cs, cacheLine)
	}
}

func TestBucketIndexBoundaries(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0},
		{-5 * time.Second, 0}, // negative durations must not panic/misbehave
		{999 * time.Nanosecond, 0},
		{1000 * time.Nanosecond, 0}, // [1000,2000) -> bucket 0
		{1999 * time.Nanosecond, 0},
		{2000 * time.Nanosecond, 1}, // [2000,4000) -> bucket 1
		{3999 * time.Nanosecond, 1},
		{4000 * time.Nanosecond, 2},      // [4000,8000) -> bucket 2
		{16777216000, histFiniteBuckets}, // == top finite boundary -> overflow
		{100 * time.Second, histFiniteBuckets},
	}
	for _, c := range cases {
		if got := bucketIndex(c.d); got != c.want {
			t.Errorf("bucketIndex(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}

func TestScrapeKnownSamples(t *testing.T) {
	resetForTest()
	defer resetForTest()

	// 50 samples in bucket0 [1000,2000)ns, 30 in bucket1 [2000,4000)ns,
	// 15 in bucket2 [4000,8000)ns, 4 in bucket3 [8000,16000)ns, and one
	// outlier at exactly 1s to pin down p999 and Max precisely.
	for i := 0; i < 50; i++ {
		RecordLatency(1500 * time.Nanosecond)
	}
	for i := 0; i < 30; i++ {
		RecordLatency(3000 * time.Nanosecond)
	}
	for i := 0; i < 15; i++ {
		RecordLatency(6000 * time.Nanosecond)
	}
	for i := 0; i < 4; i++ {
		RecordLatency(12000 * time.Nanosecond)
	}
	RecordLatency(1 * time.Second)

	snap := Scrape()

	if snap.Count != 100 {
		t.Fatalf("Count = %d, want 100", snap.Count)
	}
	if want := 2 * time.Microsecond; snap.P50 != want {
		t.Errorf("P50 = %v, want %v", snap.P50, want)
	}
	if want := 8 * time.Microsecond; snap.P90 != want {
		t.Errorf("P90 = %v, want %v", snap.P90, want)
	}
	if want := 16 * time.Microsecond; snap.P99 != want {
		t.Errorf("P99 = %v, want %v", snap.P99, want)
	}
	// The single 1s outlier is the 100th of 100 samples, so it decides
	// p999. Its own bucket's upper bound is 1000ns<<20 = 1.048576s.
	if want := time.Duration(1000) << 20; snap.P999 != want {
		t.Errorf("P999 = %v, want %v", snap.P999, want)
	}
	// Max is tracked exactly, not bucket-approximated.
	if want := time.Second; snap.Max != want {
		t.Errorf("Max = %v, want exact %v", snap.Max, want)
	}
}

func TestScrapeEmpty(t *testing.T) {
	resetForTest()
	defer resetForTest()
	snap := Scrape()
	if snap.Count != 0 || snap.P50 != 0 || snap.P99 != 0 || snap.Max != 0 {
		t.Errorf("empty snapshot not all-zero: %+v", snap)
	}
}

func TestCountersAndErrors(t *testing.T) {
	resetForTest()
	defer resetForTest()

	IncRequest()
	IncRequest()
	AddBytesCopied(100)
	AddBytesCopied(28)
	IncUpstreamDial()
	IncPoolHit()
	IncPoolHit()
	IncPoolMiss()
	IncError(ErrUpstreamTimeout)
	IncError(ErrUpstreamTimeout)
	IncError(ErrClass(200)) // out-of-range class must fold into ErrOther, not panic

	snap := Scrape()
	if snap.Requests != 2 {
		t.Errorf("Requests = %d, want 2", snap.Requests)
	}
	if snap.BytesCopied != 128 {
		t.Errorf("BytesCopied = %d, want 128", snap.BytesCopied)
	}
	if snap.UpstreamDials != 1 {
		t.Errorf("UpstreamDials = %d, want 1", snap.UpstreamDials)
	}
	if snap.PoolHits != 2 {
		t.Errorf("PoolHits = %d, want 2", snap.PoolHits)
	}
	if snap.PoolMisses != 1 {
		t.Errorf("PoolMisses = %d, want 1", snap.PoolMisses)
	}
	if snap.Errors[ErrUpstreamTimeout] != 2 {
		t.Errorf("Errors[ErrUpstreamTimeout] = %d, want 2", snap.Errors[ErrUpstreamTimeout])
	}
	if snap.Errors[ErrOther] != 1 {
		t.Errorf("Errors[ErrOther] = %d, want 1 (out-of-range class folded in)", snap.Errors[ErrOther])
	}
}

func TestEnabledToggleAndSampling(t *testing.T) {
	resetForTest()
	defer resetForTest()

	if Enabled() {
		t.Fatal("Enabled() true before SetEnabled(true)")
	}
	if ShouldSample() {
		t.Fatal("ShouldSample() true while disabled")
	}

	SetEnabled(true)
	if !Enabled() {
		t.Fatal("Enabled() false after SetEnabled(true)")
	}
	SetSampleRate(4)
	if got := SampleRate(); got != 4 {
		t.Fatalf("SampleRate() = %d, want 4", got)
	}

	hits := 0
	for i := 0; i < 400; i++ {
		if ShouldSample() {
			hits++
		}
	}
	if hits != 100 {
		t.Errorf("1-in-4 sampling over 400 calls hit %d times, want 100", hits)
	}

	SetEnabled(false)
	if ShouldSample() {
		t.Fatal("ShouldSample() true after disabling")
	}
}

func TestWriteText(t *testing.T) {
	resetForTest()
	defer resetForTest()
	IncRequest()
	RecordLatency(5 * time.Microsecond)
	snap := Scrape()

	var sb strings.Builder
	if err := snap.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"railgun_instrumentation_enabled",
		"railgun_requests_total 1",
		"railgun_latency_samples_total 1",
		"railgun_errors_total{class=\"upstream_dial\"} 0",
		"railgun_latency_seconds{quantile=\"0.5\"}",
		"railgun_latency_seconds_max",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteText output missing %q; got:\n%s", want, out)
		}
	}
}

func TestAllocsRecordPathEnabled(t *testing.T) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)
	SetSampleRate(1)

	allocs := testing.AllocsPerRun(1000, func() {
		if ShouldSample() {
			RecordLatency(5 * time.Microsecond)
		}
	})
	if allocs != 0 {
		t.Errorf("enabled record path: %v allocs/op, want 0", allocs)
	}
}

func TestAllocsRecordPathDisabled(t *testing.T) {
	resetForTest()
	defer resetForTest()
	SetEnabled(false)

	allocs := testing.AllocsPerRun(1000, func() {
		if ShouldSample() {
			RecordLatency(5 * time.Microsecond)
		}
	})
	if allocs != 0 {
		t.Errorf("disabled record path: %v allocs/op, want 0", allocs)
	}
}

func TestAllocsCounters(t *testing.T) {
	resetForTest()
	defer resetForTest()

	allocs := testing.AllocsPerRun(1000, func() {
		IncRequest()
		AddBytesCopied(128)
		IncUpstreamDial()
		IncPoolHit()
		IncPoolMiss()
		IncError(ErrOther)
	})
	if allocs != 0 {
		t.Errorf("plain counters: %v allocs/op, want 0", allocs)
	}
}

// TestConcurrentRecordAndScrape exercises Record*/Inc*/Scrape from many
// goroutines at once. Run with -race: writers only ever touch their own
// shard's atomics (procPin excludes concurrent writers from the same
// shard) and Scrape only ever uses atomic loads, so this must be clean.
func TestConcurrentRecordAndScrape(t *testing.T) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)
	SetSampleRate(1)

	const workers = 16
	const perWorker = 2000

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				if ShouldSample() {
					RecordLatency(time.Duration(j%5000+1) * time.Microsecond)
				}
				IncRequest()
				AddBytesCopied(64)
				IncUpstreamDial()
				IncPoolHit()
				IncPoolMiss()
				IncError(ErrClass((w + j) % int(numErrClasses)))
			}
		}(w)
	}

	stop := make(chan struct{})
	scrapeDone := make(chan struct{})
	go func() {
		defer close(scrapeDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = Scrape()
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-scrapeDone

	snap := Scrape()
	if snap.Requests != uint64(workers*perWorker) {
		t.Errorf("Requests = %d, want %d", snap.Requests, workers*perWorker)
	}
	if snap.BytesCopied != uint64(workers*perWorker*64) {
		t.Errorf("BytesCopied = %d, want %d", snap.BytesCopied, workers*perWorker*64)
	}
}
