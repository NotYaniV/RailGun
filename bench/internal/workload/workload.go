// Package workload defines the benchmark workloads from spec §4.4 as data, and
// renders each into the exact load-generator argv it runs. The argv is recorded
// alongside results so a run can be reproduced exactly.
package workload

import (
	"strconv"
	"time"
)

type Workload struct {
	Name        string
	Description string

	// Threads and Connections configure wrk2. Connections is load-bearing for
	// W2 specifically: see spec §4.4.
	Threads     int
	Connections int

	// Rate is requests/sec. Fixed-rate load is mandatory (spec §4.3).
	Rate     int
	Duration time.Duration

	// Paths are origin paths the workload requests. Multiple entries mean a
	// mixed workload; the runner supplies them to wrk2 via a Lua script.
	Paths []string

	// HeavyRatio is the fraction of connections assigned the heavy path in a
	// CPU-skewed workload. Zero for uniform workloads.
	HeavyRatio float64

	// Keepalive false forces a new connection per request (W3).
	Keepalive bool
}

func All() []Workload {
	return []Workload{
		{
			Name:        "W1",
			Description: "small uniform: 1 KiB responses, identical fast upstreams. Primarily an RPS test.",
			Threads:     8,
			Connections: 256,
			Rate:        50000,
			Duration:    60 * time.Second,
			Paths:       []string{"/bytes/1024"},
			Keepalive:   true,
		},
		{
			Name: "W2",
			Description: "CPU-skewed: ~10% heavy connections (1 MiB) against 90% light (1 KiB), " +
				"at deliberately low connection count so the SO_REUSEPORT hash can land unevenly. " +
				"This is the H1 test (spec §5.1.1).",
			Threads:     8,
			Connections: 64,
			Rate:        5000,
			Duration:    60 * time.Second,
			Paths:       []string{"/bytes/1048576", "/bytes/1024"},
			HeavyRatio:  0.1,
			Keepalive:   true,
		},
		{
			Name:        "W3",
			Description: "connection churn: no downstream keepalive, forcing accept-path cost to dominate.",
			Threads:     8,
			Connections: 256,
			Rate:        20000,
			Duration:    60 * time.Second,
			Paths:       []string{"/bytes/1024"},
			Keepalive:   false,
		},
		{
			Name:        "W4",
			Description: "large body: 1 MiB responses, exercising the splice path (spec §5.5, experiment E2).",
			Threads:     8,
			Connections: 64,
			Rate:        2000,
			Duration:    60 * time.Second,
			Paths:       []string{"/bytes/1048576"},
			Keepalive:   true,
		},
	}
}

func ByName(name string) (Workload, bool) {
	for _, w := range All() {
		if w.Name == name {
			return w, true
		}
	}
	return Workload{}, false
}

// WrkArgs renders the full argv, including argv[0], for this workload against
// targetURL. Callers record the result verbatim in the run record.
func (w Workload) WrkArgs(targetURL string) []string {
	args := []string{
		"wrk2",
		"-t", strconv.Itoa(w.Threads),
		"-c", strconv.Itoa(w.Connections),
		"-d", w.Duration.String(),
		"-R", strconv.Itoa(w.Rate),
		"--latency",
	}
	if !w.Keepalive {
		args = append(args, "-H", "Connection: close")
	}
	if len(w.Paths) == 1 {
		args = append(args, "--script", "bench/wrk/single.lua")
	} else {
		args = append(args, "--script", "bench/wrk/mixed.lua")
	}
	return append(args, targetURL)
}
