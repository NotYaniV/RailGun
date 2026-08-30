package agg

import (
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/result"
	"github.com/NotYaniV/RailGun/bench/internal/wrk"
)

func runs(arm string, p999 ...time.Duration) []result.Run {
	out := make([]result.Run, 0, len(p999))
	for i, d := range p999 {
		out = append(out, result.Run{
			Arm: arm, Workload: "W1", Repeat: i + 1,
			RPS: 50000, Requests: 3_000_000, CPUSeconds: 120,
			EndToEnd:  wrk.Latency{P999: d},
			PerWorker: []float64{30, 30, 30, 30},
		})
	}
	return out
}

func TestSummarizeMedianAndSpread(t *testing.T) {
	in := runs("nginx", 10*time.Millisecond, 12*time.Millisecond, 11*time.Millisecond,
		14*time.Millisecond, 11*time.Millisecond)
	got := Summarize(in)
	if len(got) != 1 {
		t.Fatalf("Summarize() returned %d summaries, want 1", len(got))
	}
	s := got[0]
	if s.N != 5 {
		t.Errorf("N = %d, want 5", s.N)
	}
	if s.P999Median != 11*time.Millisecond {
		t.Errorf("P999Median = %v, want 11ms", s.P999Median)
	}
	if s.P999Spread != 4*time.Millisecond { // max 14 - min 10
		t.Errorf("P999Spread = %v, want 4ms", s.P999Spread)
	}
}

// Spec §4.2: an effect smaller than the rig's noise floor is not a result.
func TestVerdictRefusesEffectBelowNoiseFloor(t *testing.T) {
	got := Verdict(30*time.Microsecond, 5*time.Microsecond, 200*time.Microsecond)
	if got != "below-noise-floor" {
		t.Errorf("Verdict() = %q, want %q", got, "below-noise-floor")
	}
}

// Spec §4.2: a result whose inter-run spread exceeds the effect size is not a
// result either.
func TestVerdictRefusesEffectBelowRunVariance(t *testing.T) {
	got := Verdict(300*time.Microsecond, 900*time.Microsecond, 100*time.Microsecond)
	if got != "below-run-variance" {
		t.Errorf("Verdict() = %q, want %q", got, "below-run-variance")
	}
}

func TestVerdictAcceptsCleanEffect(t *testing.T) {
	got := Verdict(2*time.Millisecond, 200*time.Microsecond, 150*time.Microsecond)
	if got != "measurable" {
		t.Errorf("Verdict() = %q, want %q", got, "measurable")
	}
}

func TestSummarizeGroupsByArmAndWorkload(t *testing.T) {
	in := append(runs("nginx", 10*time.Millisecond), runs("null", 1*time.Millisecond)...)
	got := Summarize(in)
	if len(got) != 2 {
		t.Fatalf("Summarize() returned %d summaries, want 2", len(got))
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if got := Summarize(nil); len(got) != 0 {
		t.Errorf("Summarize(nil) returned %d summaries, want 0", len(got))
	}
}
