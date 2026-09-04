package metrics

import (
	"testing"
	"time"
)

// BenchmarkShouldSample_Disabled measures the fully-off path: this is
// what a peak-RPS run pays per request. Per spec §5.8 it must be a
// single predictable-branch atomic load and return — no time.Now().
func BenchmarkShouldSample_Disabled(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ShouldSample()
	}
}

// BenchmarkShouldSample_Enabled measures the sampling-decision cost
// while instrumentation is on (1-in-1, i.e. every call samples).
func BenchmarkShouldSample_Enabled(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)
	SetSampleRate(1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ShouldSample()
	}
}

// BenchmarkRecordLatency measures the enabled record path itself
// (bucket lookup + pinned atomic increment + max CAS), excluding any
// time.Now() call — that cost belongs to the caller, not this package.
func BenchmarkRecordLatency(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		RecordLatency(5 * time.Microsecond)
	}
}

// BenchmarkRecordLatency_Parallel is the same path under concurrency,
// to show shard contention (or the lack of it) across cores.
func BenchmarkRecordLatency_Parallel(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			RecordLatency(5 * time.Microsecond)
		}
	})
}

// BenchmarkFullRecordRoundTrip_Enabled approximates the caller's real
// usage pattern (check -> maybe record) with instrumentation on.
func BenchmarkFullRecordRoundTrip_Enabled(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)
	SetSampleRate(1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ShouldSample() {
			RecordLatency(5 * time.Microsecond)
		}
	}
}

// BenchmarkFullRecordRoundTrip_Disabled is the same call pattern with
// instrumentation off: this is the number that must stay near zero.
func BenchmarkFullRecordRoundTrip_Disabled(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ShouldSample() {
			RecordLatency(5 * time.Microsecond)
		}
	}
}

func BenchmarkIncRequest(b *testing.B) {
	resetForTest()
	defer resetForTest()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		IncRequest()
	}
}

func BenchmarkAddBytesCopied(b *testing.B) {
	resetForTest()
	defer resetForTest()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AddBytesCopied(1460)
	}
}

func BenchmarkIncError(b *testing.B) {
	resetForTest()
	defer resetForTest()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		IncError(ErrUpstreamTimeout)
	}
}

// BenchmarkScrape measures the merge cost, which runs once per
// collection interval, not per request, so it is allowed to be slow.
func BenchmarkScrape(b *testing.B) {
	resetForTest()
	defer resetForTest()
	SetEnabled(true)
	for i := 0; i < 10000; i++ {
		RecordLatency(time.Duration(i%9973+1) * time.Microsecond)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Scrape()
	}
}
