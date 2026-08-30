// Package perf parses the CSV output of `perf stat -x,`.
package perf

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Events is the counter set spec §4.5 requires on every run.
var Events = []string{
	"cycles",
	"LLC-load-misses",
	"LLC-loads",
	"raw_syscalls:sys_enter",
	"syscalls:sys_enter_splice",
}

// Parse reads `perf stat -x,` output. Events the PMU could not schedule
// (reported as "<not counted>" or "<not supported>") are omitted from the
// result rather than recorded as zero, so a missing counter is distinguishable
// from a genuinely zero one.
func Parse(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
		if err != nil {
			continue // "<not counted>", "<not supported>"
		}
		if name := strings.TrimSpace(f[2]); name != "" {
			out[name] = v
		}
	}
	return out, sc.Err()
}
