package report

import (
	"strings"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/agg"
)

func TestRenderIncludesNoiseFloorAndVerdict(t *testing.T) {
	sums := []agg.Summary{
		{Arm: "null", Workload: "W1", N: 5, P999Median: 200 * time.Microsecond, P999Spread: 40 * time.Microsecond},
		{Arm: "nginx", Workload: "W1", N: 5, RPSMedian: 50000, RPSSpread: 300,
			P999Median: 12 * time.Millisecond, P999Spread: 900 * time.Microsecond,
			CPUPerMillionMedian: 40.2, CPUSkew: 1.02},
	}
	var sb strings.Builder
	if err := Render(&sb, sums); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := sb.String()

	for _, want := range []string{"Noise floor", "nginx", "W1", "measurable", "CPU skew"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

// The null arm is the whole basis for reporting deltas (spec §4.2). A report
// generated without one is not publishable and must say so loudly rather than
// quietly printing absolute numbers.
func TestRenderWithoutNullArmIsRefused(t *testing.T) {
	sums := []agg.Summary{{Arm: "nginx", Workload: "W1", N: 5, P999Median: time.Millisecond}}
	var sb strings.Builder
	err := Render(&sb, sums)
	if err == nil {
		t.Fatal("Render() without a null arm returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("error = %q, want it to name the missing null arm", err)
	}
}

// Spec §4.2 requires at least 5 repeats; a report built from fewer must be
// marked, not silently published.
func TestRenderFlagsUnderpoweredRuns(t *testing.T) {
	sums := []agg.Summary{
		{Arm: "null", Workload: "W1", N: 5, P999Median: 200 * time.Microsecond},
		{Arm: "nginx", Workload: "W1", N: 2, P999Median: 12 * time.Millisecond},
	}
	var sb strings.Builder
	if err := Render(&sb, sums); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(sb.String(), "UNDERPOWERED") {
		t.Errorf("report must flag n<5 runs as UNDERPOWERED, got:\n%s", sb.String())
	}
}
