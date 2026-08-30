package main

type step struct {
	Arm      string
	Workload string
	Repeat   int
}

// runOrder interleaves arms so that a given repeat of every arm runs before any
// arm's next repeat.
//
// Blocked ordering (all of arm A, then all of arm B) confounds arm with time:
// a noisy neighbour, a thermal shift, or a network change during the session
// lands entirely on one arm and is indistinguishable from a real effect.
// Alternating spreads any such drift across both arms.
func runOrder(arms, workloads []string, repeats int) []step {
	var out []step
	for r := 1; r <= repeats; r++ {
		for _, wl := range workloads {
			for _, arm := range arms {
				out = append(out, step{Arm: arm, Workload: wl, Repeat: r})
			}
		}
	}
	return out
}
