package result

import (
	"math"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/wrk"
)

func sample() Run {
	return Run{
		Schema:     1,
		RunID:      "20260830-210000-nginx-W1-1",
		Timestamp:  time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC),
		Arm:        "nginx",
		Workload:   "W1",
		Repeat:     1,
		RPS:        49997.85,
		Requests:   2999871,
		CPUSeconds: 120.5,
		EndToEnd:   wrk.Latency{P50: time.Millisecond, P999: 12 * time.Millisecond},
		Counters:   map[string]float64{"cycles": 4.12e11},
		PerWorker:  []float64{30.1, 30.2, 30.1, 30.1},
		WrkArgv:    []string{"wrk2", "-R", "50000", "http://x"},
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, sample()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadDir() returned %d runs, want 1", len(got))
	}
	if got[0].RunID != sample().RunID || got[0].RPS != sample().RPS {
		t.Errorf("round trip lost data: got %+v", got[0])
	}
	if got[0].EndToEnd.P999 != 12*time.Millisecond {
		t.Errorf("P999 = %v, want 12ms", got[0].EndToEnd.P999)
	}
}

func TestCPUSecondsPerMillion(t *testing.T) {
	r := sample()
	got := r.CPUSecondsPerMillion()

	// Computed in float64 rather than written as a constant expression: Go
	// evaluates untyped constant arithmetic at arbitrary precision, so a
	// literal expectation differs from the runtime result by an ULP.
	want := 120.5 / (float64(2999871) / 1e6)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("CPUSecondsPerMillion() = %v, want %v", got, want)
	}
}

func TestCPUSecondsPerMillionZeroRequests(t *testing.T) {
	r := sample()
	r.Requests = 0
	if got := r.CPUSecondsPerMillion(); got != 0 {
		t.Errorf("with zero requests got %v, want 0 (must not divide by zero)", got)
	}
}
