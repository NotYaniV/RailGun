package perf

import (
	"os"
	"strings"
	"testing"
)

func TestParseSample(t *testing.T) {
	f, err := os.Open("../../testdata/perf-sample.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := map[string]float64{
		"cycles":                    412938174562,
		"LLC-load-misses":           2871234,
		"LLC-loads":                 198234112,
		"raw_syscalls:sys_enter":    14992355,
		"syscalls:sys_enter_splice": 2999871,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// "<not counted>" means the PMU could not schedule the event. Recording it as
// 0 would silently understate a metric — worse than admitting it is absent.
func TestNotCountedIsOmittedNotZero(t *testing.T) {
	f, _ := os.Open("../../testdata/perf-sample.csv")
	defer f.Close()
	got, _ := Parse(f)
	if _, present := got["uncore_imc/data_reads/"]; present {
		t.Error("<not counted> event was recorded; it must be omitted, not stored as 0")
	}
}

func TestParseIgnoresCommentaryLines(t *testing.T) {
	in := "# started on Sat Aug 30 21:00:00 2026\n\n1234,,cycles,1000,100.00,,\n"
	got, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got["cycles"] != 1234 {
		t.Errorf("cycles = %v, want 1234", got["cycles"])
	}
}
