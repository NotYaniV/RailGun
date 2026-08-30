package wrk

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseSample(t *testing.T) {
	f, err := os.Open("../../testdata/wrk2-sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if got.RPS != 49997.85 {
		t.Errorf("RPS = %v, want 49997.85", got.RPS)
	}
	if got.Requests != 2999871 {
		t.Errorf("Requests = %d, want 2999871", got.Requests)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"P50", got.Latency.P50, 1200 * time.Microsecond},
		{"P90", got.Latency.P90, 2400 * time.Microsecond},
		{"P99", got.Latency.P99, 5620 * time.Microsecond},
		{"P999", got.Latency.P999, 12310 * time.Microsecond},
		{"Max", got.Latency.Max, 21060 * time.Microsecond},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A truncated run (generator crashed, target refused connections) must be a
// hard error. Silently returning a zero-valued Output would let a dead run
// enter the aggregate as a real data point.
func TestParseTruncatedIsError(t *testing.T) {
	in := "Running 1m test @ http://10.0.0.5:8080\n  8 threads and 256 connections\n"
	if _, err := Parse(strings.NewReader(in)); err == nil {
		t.Error("Parse() on truncated output returned nil error, want an error")
	}
}
