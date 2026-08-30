// Package agg turns raw runs into reportable summaries, and refuses to report
// effects the rig cannot actually resolve.
package agg

import (
	"sort"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/proc"
	"github.com/NotYaniV/RailGun/bench/internal/result"
)

type Summary struct {
	Arm      string
	Workload string
	N        int

	RPSMedian  float64
	RPSSpread  float64
	P999Median time.Duration
	P999Spread time.Duration
	P99Median  time.Duration

	CPUPerMillionMedian float64
	CPUSkew             float64
}

// Summarize groups runs by (arm, workload) and reduces each group to medians
// and spreads. Spread is max-min, not standard deviation: with n=5 a standard
// deviation is not meaningfully estimable, and the honest question is how far
// apart the runs actually landed.
func Summarize(runs []result.Run) []Summary {
	type key struct{ arm, wl string }
	groups := map[key][]result.Run{}
	var order []key
	for _, r := range runs {
		k := key{r.Arm, r.Workload}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}

	out := make([]Summary, 0, len(order))
	for _, k := range order {
		g := groups[k]
		rps := make([]float64, len(g))
		cpm := make([]float64, len(g))
		p999 := make([]time.Duration, len(g))
		p99 := make([]time.Duration, len(g))
		var skewSum float64
		for i, r := range g {
			rps[i] = r.RPS
			cpm[i] = r.CPUSecondsPerMillion()
			p999[i] = r.EndToEnd.P999
			p99[i] = r.EndToEnd.P99
			skewSum += proc.CPUSkew(r.PerWorker)
		}
		out = append(out, Summary{
			Arm:                 k.arm,
			Workload:            k.wl,
			N:                   len(g),
			RPSMedian:           medianFloat(rps),
			RPSSpread:           spreadFloat(rps),
			P999Median:          medianDur(p999),
			P999Spread:          spreadDur(p999),
			P99Median:           medianDur(p99),
			CPUPerMillionMedian: medianFloat(cpm),
			CPUSkew:             skewSum / float64(len(g)),
		})
	}
	return out
}

// Verdict decides whether a measured effect may be reported as real.
//
// Two independent gates, both from spec §4.2. The rig's noise floor bounds what
// the hardware can resolve at all; the inter-run spread bounds what this
// particular set of runs can resolve. An effect must clear both.
func Verdict(effect, spread, floor time.Duration) string {
	if effect < 0 {
		effect = -effect
	}
	switch {
	case effect < floor:
		return "below-noise-floor"
	case effect < spread:
		return "below-run-variance"
	default:
		return "measurable"
	}
}

func medianFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func spreadFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)-1] - s[0]
}

func medianDur(v []time.Duration) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func spreadDur(v []time.Duration) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)-1] - s[0]
}
