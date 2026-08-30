// Package result defines the on-disk record for a single benchmark run.
//
// One JSON file per run, never appended to and never mutated. Aggregation reads
// the directory. This keeps a partially-written or crashed run from corrupting
// results that were already collected.
package result

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/wrk"
)

// Schema is bumped whenever Run's shape changes, so old result directories
// remain interpretable rather than being silently misread.
const Schema = 1

type Run struct {
	Schema    int       `json:"schema"`
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"timestamp"`

	// Arm is "nginx", "railgun", or "null". "null" is the noise-floor arm
	// required by spec §4.2 and must be present in every session.
	Arm      string `json:"arm"`
	Workload string `json:"workload"`
	Repeat   int    `json:"repeat"`

	RPS      float64 `json:"rps"`
	Requests uint64  `json:"requests"`

	EndToEnd wrk.Latency  `json:"end_to_end"`
	Internal *wrk.Latency `json:"internal,omitempty"`

	CPUSeconds float64            `json:"cpu_seconds"`
	RSSBytes   uint64             `json:"rss_bytes"`
	Counters   map[string]float64 `json:"counters"`
	PerWorker  []float64          `json:"per_worker_cpu"`

	// WrkArgv is recorded verbatim so a run can be reproduced exactly.
	WrkArgv []string `json:"wrk_argv"`

	// Notes carries rig conditions worth preserving with the numbers.
	Notes string `json:"notes,omitempty"`
}

// CPUSecondsPerMillion distinguishes "we are faster" from "we were given more
// cores" (spec §4.5).
func (r Run) CPUSecondsPerMillion() float64 {
	if r.Requests == 0 {
		return 0
	}
	return r.CPUSeconds / (float64(r.Requests) / 1e6)
}

func Write(dir string, r Run) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.RunID+".json")
	return path, os.WriteFile(path, b, 0o644)
}

func LoadDir(dir string) ([]Run, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r Run
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}
