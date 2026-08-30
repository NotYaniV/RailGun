// Package wrk parses wrk2 stdout into structured results.
//
// wrk2's HdrHistogram block is the only trustworthy latency source in its
// output: the "Thread Stats" Avg/Max line is computed per thread and does not
// correct for coordinated omission.
package wrk

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Latency struct {
	P50, P90, P99, P999, Max time.Duration
}

type Output struct {
	RPS      float64
	Requests uint64
	Latency  Latency
}

func Parse(r io.Reader) (Output, error) {
	var out Output
	var sawRPS, sawRequests bool
	pct := map[string]time.Duration{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		switch {
		case strings.HasPrefix(line, "Requests/sec:"):
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "Requests/sec:")), 64)
			if err != nil {
				return out, fmt.Errorf("parsing Requests/sec: %w", err)
			}
			out.RPS, sawRPS = v, true

		case strings.Contains(line, " requests in "):
			// e.g. "2999871 requests in 1.00m, 3.05GB read"
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			n, err := strconv.ParseUint(f[0], 10, 64)
			if err != nil {
				return out, fmt.Errorf("parsing request count: %w", err)
			}
			out.Requests, sawRequests = n, true

		default:
			// percentile rows: " 99.900%   12.31ms"
			f := strings.Fields(line)
			if len(f) != 2 || !strings.HasSuffix(f[0], "%") {
				continue
			}
			d, err := parseDur(f[1])
			if err != nil {
				continue
			}
			pct[strings.TrimSuffix(f[0], "%")] = d
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}

	if !sawRPS || !sawRequests {
		return out, fmt.Errorf("wrk2 output incomplete: missing summary lines (run likely died mid-flight)")
	}
	for _, key := range []string{"50.000", "90.000", "99.000", "99.900", "100.000"} {
		if _, ok := pct[key]; !ok {
			return out, fmt.Errorf("wrk2 output missing %s%% percentile; was --latency passed?", key)
		}
	}
	out.Latency = Latency{
		P50:  pct["50.000"],
		P90:  pct["90.000"],
		P99:  pct["99.000"],
		P999: pct["99.900"],
		Max:  pct["100.000"],
	}
	return out, nil
}

// parseDur handles wrk2's duration spellings, which include "us" — a unit
// time.ParseDuration does not accept.
func parseDur(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "us") {
		s = strings.TrimSuffix(s, "us") + "µs"
	}
	return time.ParseDuration(s)
}
