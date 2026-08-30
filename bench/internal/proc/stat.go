// Package proc reads per-process CPU time from /proc, which is how per-worker
// CPU distribution — the direct measurement of H1 (spec §5.1.1) — is collected.
package proc

import (
	"bytes"
	"fmt"
	"strconv"
)

// ParseStat extracts utime and stime (in clock ticks) from the contents of
// /proc/<pid>/stat.
//
// Field 2 is the executable name in parentheses and may itself contain both
// spaces and parentheses ("nginx: worker (x)"). Splitting the whole line on
// whitespace is therefore wrong; everything must be indexed relative to the
// LAST ')'.
func ParseStat(b []byte) (utime, stime uint64, err error) {
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return 0, 0, fmt.Errorf("proc: no comm field terminator in stat line")
	}
	// After ")": field 3 (state) onward. utime is field 14, stime field 15,
	// so they are offsets 11 and 12 in this remainder.
	f := bytes.Fields(b[end+1:])
	if len(f) < 13 {
		return 0, 0, fmt.Errorf("proc: stat line has %d fields after comm, want >= 13", len(f))
	}
	if utime, err = strconv.ParseUint(string(f[11]), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("proc: parsing utime: %w", err)
	}
	if stime, err = strconv.ParseUint(string(f[12]), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("proc: parsing stime: %w", err)
	}
	return utime, stime, nil
}

// CPUSkew returns max/mean of per-worker CPU time. 1.0 is perfectly even.
//
// This decides whether W2's premise actually fired: if NGINX shows no skew, the
// 4-tuple hash distributed evenly and the workload measured nothing, which per
// spec §11 risk 3 means lowering the connection count — not reporting a win.
func CPUSkew(perWorker []float64) float64 {
	if len(perWorker) == 0 {
		return 1.0
	}
	var sum, max float64
	for _, v := range perWorker {
		sum += v
		if v > max {
			max = v
		}
	}
	mean := sum / float64(len(perWorker))
	if mean == 0 {
		return 1.0
	}
	return max / mean
}
