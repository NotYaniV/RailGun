//go:build linux

package proc

import (
	"fmt"
	"os"
)

// ClockTick is the kernel's USER_HZ. It is 100 on every Linux target this
// project supports; the harness records raw ticks and converts here so the
// assumption lives in one place.
const ClockTick = 100

// SampleWorkers returns cumulative CPU seconds for each pid, in the order given.
func SampleWorkers(pids []int) ([]float64, error) {
	out := make([]float64, 0, len(pids))
	for _, pid := range pids {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, fmt.Errorf("proc: reading pid %d: %w", pid, err)
		}
		u, s, err := ParseStat(b)
		if err != nil {
			return nil, fmt.Errorf("proc: pid %d: %w", pid, err)
		}
		out = append(out, float64(u+s)/ClockTick)
	}
	return out, nil
}
