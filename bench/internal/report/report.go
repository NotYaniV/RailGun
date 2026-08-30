// Package report renders aggregated summaries as markdown.
package report

import (
	"fmt"
	"io"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/agg"
)

// minRepeats mirrors spec §4.2. Reports built from fewer runs are still
// rendered — suppressing them entirely would hide work in progress — but every
// affected row is marked so an underpowered number cannot be quoted as final.
const minRepeats = 5

func Render(w io.Writer, sums []agg.Summary) error {
	floors := map[string]agg.Summary{} // workload -> null arm
	for _, s := range sums {
		if s.Arm == "null" {
			floors[s.Workload] = s
		}
	}
	if len(floors) == 0 {
		return fmt.Errorf("report: no null arm present; every session must measure the " +
			"noise floor, because results are reported as deltas over it (spec §4.2)")
	}

	fmt.Fprintf(w, "# Benchmark report\n\n")
	fmt.Fprintf(w, "Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(w, "## Noise floor (null arm, no proxy in path)\n\n")
	fmt.Fprintf(w, "| Workload | n | p99.9 | spread |\n|---|---|---|---|\n")
	for wl, f := range floors {
		fmt.Fprintf(w, "| %s | %d | %v | %v |\n", wl, f.N, f.P999Median, f.P999Spread)
	}

	fmt.Fprintf(w, "\n## Results\n\n")
	fmt.Fprintf(w, "| Arm | Workload | n | RPS (median) | RPS spread | p99.9 | Δ over floor | verdict | CPU-s/Mreq | CPU skew |\n")
	fmt.Fprintf(w, "|---|---|---|---|---|---|---|---|---|---|\n")

	for _, s := range sums {
		if s.Arm == "null" {
			continue
		}
		floor, ok := floors[s.Workload]
		if !ok {
			fmt.Fprintf(w, "| %s | %s | %d | — | — | %v | NO FLOOR FOR THIS WORKLOAD | unreportable | — | — |\n",
				s.Arm, s.Workload, s.N, s.P999Median)
			continue
		}
		delta := s.P999Median - floor.P999Median
		verdict := agg.Verdict(delta, s.P999Spread, floor.P999Median)
		name := s.Arm
		if s.N < minRepeats {
			name += " **UNDERPOWERED**"
		}
		fmt.Fprintf(w, "| %s | %s | %d | %.0f | %.0f | %v | %v | %s | %.1f | %.3f |\n",
			name, s.Workload, s.N, s.RPSMedian, s.RPSSpread,
			s.P999Median, delta, verdict, s.CPUPerMillionMedian, s.CPUSkew)
	}

	fmt.Fprintf(w, "\n_A verdict of `below-noise-floor` or `below-run-variance` means the rig "+
		"could not resolve the effect. Such a row is not a win and must not be quoted as one._\n")
	return nil
}
