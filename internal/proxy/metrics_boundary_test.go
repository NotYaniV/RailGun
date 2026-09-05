package proxy_test

import (
	"fmt"
	"testing"

	"github.com/NotYaniV/RailGun/internal/metrics"
	"github.com/NotYaniV/RailGun/internal/proxy"
)

// The metrics package is thoroughly unit-tested in isolation: buckets,
// percentiles, shard padding, allocation counts. What none of that establishes
// is that a real request travelling through a real proxy reaches any of it.
//
// That gap matters more here than it usually would, because two of these
// counters are not diagnostics — they are the instruments the published result
// is read off. Spec 5.8 has peak-RPS runs executed with instrumentation off and
// the build reporting that it was off; spec 4.5 has the userspace-bytes-copied
// counter deciding experiment E2. An instrument that silently reads zero
// produces a clean-looking run and a wrong conclusion.
//
// These tests run sequentially and take deltas, because the metrics package is
// process-global by design — per-P shards merged at scrape.

func scrapeDelta(t *testing.T, f func()) (before, after metrics.Snapshot) {
	t.Helper()
	before = metrics.Scrape()
	f()
	after = metrics.Scrape()
	return before, after
}

// TestRequestsReachTheHistogram checks that an exchange actually records a
// latency sample. If it does not, every latency number in the writeup is drawn
// from an empty histogram.
func TestRequestsReachTheHistogram(t *testing.T) {
	metrics.SetEnabled(true)
	metrics.SetSampleRate(1)
	t.Cleanup(func() { metrics.SetEnabled(true); metrics.SetSampleRate(16) })

	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	const n = 20
	before, after := scrapeDelta(t, func() {
		for i := 0; i < n; i++ {
			if resp, _ := r.get(c, "/small"); resp.StatusCode != 200 {
				t.Fatalf("request %d failed", i)
			}
		}
	})

	if d := after.Requests - before.Requests; d < n {
		t.Errorf("request counter rose by %d for %d requests", d, n)
	}
	if d := after.Count - before.Count; d < n {
		t.Errorf("histogram recorded %d samples for %d requests at sample rate 1; latency numbers would come from a near-empty histogram", d, n)
	}
	if after.P50 <= 0 {
		t.Errorf("p50 is %v after %d recorded requests", after.P50, n)
	}
}

// TestInstrumentationOffRecordsNothing pins the claim spec 5.8 requires a
// peak-RPS run to make about itself.
//
// The run reports that instrumentation was off. If "off" still recorded, the
// peak-RPS number would carry the sampling cost it was specifically taken
// without, and would not be comparable to anything — including the latency runs
// it is meant to be distinguished from.
func TestInstrumentationOffRecordsNothing(t *testing.T) {
	metrics.SetEnabled(false)
	t.Cleanup(func() { metrics.SetEnabled(true); metrics.SetSampleRate(16) })

	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	const n = 20
	before, after := scrapeDelta(t, func() {
		for i := 0; i < n; i++ {
			r.get(c, "/small")
		}
	})

	if d := after.Count - before.Count; d != 0 {
		t.Errorf("histogram recorded %d samples with instrumentation off", d)
	}
	if after.InstrumentationEnabled {
		t.Error("scrape reports instrumentation enabled after it was turned off; a run cannot report its own state")
	}
	// Request counting is deliberately unconditional: a peak-RPS run still
	// needs RPS. Asserted so that a future change turning it off with the rest
	// does not quietly remove the number the run exists to produce.
	if d := after.Requests - before.Requests; d < n {
		t.Errorf("request counter rose by %d with instrumentation off; RPS must still be counted", d)
	}
}

// TestSamplingRateIsApproximatelyHonoured checks 1-in-N does roughly that under
// real traffic. A sampler that silently recorded everything would make the
// latency runs cost what they were sampled to avoid.
func TestSamplingRateIsApproximatelyHonoured(t *testing.T) {
	metrics.SetEnabled(true)
	metrics.SetSampleRate(8)
	t.Cleanup(func() { metrics.SetEnabled(true); metrics.SetSampleRate(16) })

	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	const n = 400
	before, after := scrapeDelta(t, func() {
		for i := 0; i < n; i++ {
			r.get(c, "/small")
		}
	})

	got := after.Count - before.Count
	// Wide bounds on purpose. This distinguishes "sampling works" from
	// "sampling is ignored in one direction or the other"; it is not a test of
	// the sampler's distribution, which the metrics package owns.
	lo, hi := uint64(n/8/4), uint64(n/8*4)
	if got < lo || got > hi {
		t.Errorf("recorded %d samples from %d requests at 1-in-8; expected roughly %d, allowing [%d, %d]",
			got, n, n/8, lo, hi)
	}
	t.Logf("1-in-8 over %d requests recorded %d samples", n, got)
}

// TestBytesCopiedDistinguishesTheE2Arms is the one that matters most.
//
// Experiment E2 compares splice against a forced userspace copy, and reads the
// difference partly off this counter. If it counted the same in both arms the
// experiment would report no difference and the natural conclusion would be
// "splice does not help" — a wrong answer that looks like a finding rather than
// like a broken instrument.
//
// The assertion is relative, not absolute, so it holds on both Linux and
// darwin. On Linux the splice arm moves the body kernel-to-kernel and counts
// almost nothing. On darwin there is no splice and io.Copy falls back to its own
// internal buffer, which this counter also does not see — the same low reading
// for a different reason. Either way the force-copy arm, which pushes every byte
// through a buffer this proxy owns, must count the body and must count more.
func TestBytesCopiedDistinguishesTheE2Arms(t *testing.T) {
	metrics.SetEnabled(true)
	t.Cleanup(func() { metrics.SetSampleRate(16) })

	const body = 1 << 20
	path := fmt.Sprintf("/bytes/%d", body)

	measure := func(o proxy.Options) uint64 {
		rig := newRig(t, o, echoHandler())
		before := metrics.Scrape()
		if _, b := rig.get(rig.client(), path); len(b) != body {
			t.Fatalf("arm returned %d bytes, want %d", len(b), body)
		}
		return metrics.Scrape().BytesCopied - before.BytesCopied
	}

	spliceOpts := testOpts()
	spliceOpts.SpliceThreshold = 1024
	spliceOpts.ForceCopy = false
	spliced := measure(spliceOpts)

	copyOpts := testOpts()
	copyOpts.ForceCopy = true
	copied := measure(copyOpts)

	if copied < body {
		t.Errorf("force-copy arm counted %d userspace bytes for a %d-byte body; the counter E2 reads is not seeing the copy",
			copied, body)
	}
	if spliced >= copied {
		t.Errorf("splice arm counted %d userspace bytes and force-copy arm %d; the counter does not separate the two arms, so E2 would report no difference and that would read as a finding rather than as a broken instrument",
			spliced, copied)
	}
	t.Logf("splice arm counted %d userspace bytes, force-copy arm %d, for a %d-byte body",
		spliced, copied, body)
}
