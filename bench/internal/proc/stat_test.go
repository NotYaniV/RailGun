package proc

import (
	"math"
	"testing"
)

// The comm field is parenthesised and may itself contain spaces and
// parentheses. Splitting on whitespace from the left is the classic bug here,
// so the fixture uses a hostile process name on purpose.
func TestParseStatHandlesParensInComm(t *testing.T) {
	line := []byte("4242 (nginx: worker (x)) S 1 4242 4242 0 -1 4194560 " +
		"1234 0 0 0 918 372 0 0 20 0 1 0 99887766 12345678 890 " +
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")

	utime, stime, err := ParseStat(line)
	if err != nil {
		t.Fatalf("ParseStat() error = %v", err)
	}
	if utime != 918 {
		t.Errorf("utime = %d, want 918", utime)
	}
	if stime != 372 {
		t.Errorf("stime = %d, want 372", stime)
	}
}

func TestParseStatRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no parens here", "123 (short) S 1 2"} {
		if _, _, err := ParseStat([]byte(in)); err == nil {
			t.Errorf("ParseStat(%q) returned nil error, want an error", in)
		}
	}
}

func TestCPUSkew(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"perfectly even", []float64{10, 10, 10, 10}, 1.0},
		{"one hot worker", []float64{40, 10, 10, 10}, 2.285714},
		{"all idle", []float64{0, 0, 0, 0}, 1.0},
	}
	for _, c := range cases {
		got := CPUSkew(c.in)
		if math.Abs(got-c.want) > 1e-5 {
			t.Errorf("%s: CPUSkew(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
