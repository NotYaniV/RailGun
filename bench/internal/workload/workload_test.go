package workload

import (
	"strings"
	"testing"
)

func TestAllWorkloadsPresent(t *testing.T) {
	got := All()
	want := []string{"W1", "W2", "W3", "W4"}
	if len(got) != len(want) {
		t.Fatalf("All() returned %d workloads, want %d", len(got), len(want))
	}
	for i, w := range got {
		if w.Name != want[i] {
			t.Errorf("workload %d = %q, want %q", i, w.Name, want[i])
		}
		if w.Rate <= 0 {
			t.Errorf("%s: Rate = %d, want > 0 (fixed-rate load is mandatory, spec §4.3)", w.Name, w.Rate)
		}
		if w.Duration <= 0 {
			t.Errorf("%s: Duration = %v, want > 0", w.Name, w.Duration)
		}
	}
}

// Spec §4.4: W2's connection count must stay low, or the SO_REUSEPORT 4-tuple
// hash balances by law of large numbers and the workload measures nothing.
func TestW2ConnectionCountStaysLow(t *testing.T) {
	w, ok := ByName("W2")
	if !ok {
		t.Fatal("W2 not found")
	}
	if w.Connections > 256 {
		t.Errorf("W2 Connections = %d, want <= 256: high connection counts hide "+
			"the accept-time imbalance H1 predicts (spec §4.4)", w.Connections)
	}
	if w.HeavyRatio <= 0 || w.HeavyRatio >= 1 {
		t.Errorf("W2 HeavyRatio = %v, want a fraction in (0,1)", w.HeavyRatio)
	}
}

func TestWrkArgsUsesFixedRate(t *testing.T) {
	w, _ := ByName("W1")
	args := w.WrkArgs("http://10.0.0.5:8080")
	joined := strings.Join(args, " ")

	if args[0] != "wrk2" {
		t.Errorf("argv[0] = %q, want %q", args[0], "wrk2")
	}
	if !strings.Contains(joined, "-R") {
		t.Error("argv must contain -R: open-loop load produces coordinated omission (spec §4.3)")
	}
	if !strings.Contains(joined, "--latency") {
		t.Error("argv must contain --latency to emit the HdrHistogram distribution")
	}
	if !strings.HasSuffix(joined, "http://10.0.0.5:8080") {
		t.Errorf("argv must end with the target URL, got %q", joined)
	}
}

func TestByNameUnknown(t *testing.T) {
	if _, ok := ByName("W99"); ok {
		t.Error("ByName(\"W99\") returned ok, want false")
	}
}
