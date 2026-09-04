// Command bench drives the benchmark rig.
//
// It runs on rig-load: wrk2 executes locally, while proxy-side counters are
// collected from rig-proxy over SSH. Keeping the load generator and the proxy
// on separate hosts is a requirement, not a convenience (spec §4.1).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/NotYaniV/RailGun/bench/internal/agg"
	"github.com/NotYaniV/RailGun/bench/internal/perf"
	"github.com/NotYaniV/RailGun/bench/internal/proc"
	"github.com/NotYaniV/RailGun/bench/internal/report"
	"github.com/NotYaniV/RailGun/bench/internal/result"
	"github.com/NotYaniV/RailGun/bench/internal/spliceguard"
	"github.com/NotYaniV/RailGun/bench/internal/workload"
	"github.com/NotYaniV/RailGun/bench/internal/wrk"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench <run|report> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "report":
		cmdReport(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func cmdRun(argv []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	proxyHost := fs.String("proxy-host", "", "ssh target for rig-proxy (required)")
	proxyURL := fs.String("proxy-url", "", "URL of the proxy under test, e.g. http://10.0.0.5:8080 (required)")
	nullURL := fs.String("null-url", "", "URL of the origin with no proxy in path, for the noise floor (required)")
	arm := fs.String("arm", "nginx", "name of the non-null arm: nginx or railgun")
	procName := fs.String("proc-name", "nginx", "process name whose workers are sampled on rig-proxy")
	repeats := fs.Int("repeats", 5, "repeats per (arm, workload)")
	outDir := fs.String("out", "bench/results", "directory for run records")
	scriptDir := fs.String("wrk-script-dir", "bench/wrk", "directory holding the wrk2 Lua scripts")
	remoteCmd := fs.String("remote-cmd", "ssh {host}",
		"how to run a shell command on rig-proxy; use `docker exec {host} sh -c` for a containerised dry run")
	duration := fs.Duration("duration", 0, "override each workload's duration (0 = use the workload's own); short values are for dry runs, not results")
	only := fs.String("workloads", "", "comma-separated workload names to run (default: all)")
	forceCopy := fs.Bool("force-copy", false,
		"this run is Railgun's -force-copy control arm (spec §5.5 E2): the body is routed through a "+
			"pooled userspace buffer instead of splice, so zero splice syscalls is the correct outcome "+
			"and the W4 splice guard is skipped rather than applied")
	spliceMaxCalls := fs.Float64("w4-splice-max-calls-per-request", spliceguard.DefaultMaxCallsPerRequest,
		"W4 guard: fail the run if splice() calls per large-body request rises above this "+
			"(see spliceguard.DefaultMaxCallsPerRequest for the reasoning)")
	spliceMinBytesFrac := fs.Float64("w4-splice-min-bytes-fraction", spliceguard.DefaultMinBytesPerSpliceFraction,
		"W4 guard: fail the run if average bytes moved per splice() call falls below this fraction "+
			"of the body size (see spliceguard.DefaultMinBytesPerSpliceFraction for the reasoning)")
	fs.Parse(argv)

	// proxyHost may be empty for a single-host dry run; the URLs may not.
	if *proxyURL == "" || *nullURL == "" {
		fs.Usage()
		os.Exit(2)
	}
	if *repeats < 5 {
		log.Printf("WARNING: repeats=%d is below the 5 the spec requires; "+
			"results will be marked UNDERPOWERED", *repeats)
	}

	// Preflight both targets. A full session is tens of minutes of rig time, and
	// the null arm in particular fails silently in a way that only shows up at
	// report time: if the origin is bound to loopback on rig-proxy it is
	// unreachable from here, every null run dies, and the report then refuses to
	// render for want of a noise floor. Two seconds here saves the session.
	for name, u := range map[string]string{"proxy-url": *proxyURL, "null-url": *nullURL} {
		if err := preflight(u); err != nil {
			log.Fatalf("%s is not reachable: %v\n"+
				"  the origin must bind 0.0.0.0 (not 127.0.0.1) for --null-url to work from this host", name, err)
		}
	}

	var wls []string
	for _, w := range workload.All() {
		if *only != "" && !slices.Contains(strings.Split(*only, ","), w.Name) {
			continue
		}
		wls = append(wls, w.Name)
	}
	if len(wls) == 0 {
		log.Fatalf("no workloads selected by --workloads=%q", *only)
	}
	if *duration > 0 {
		log.Printf("WARNING: --duration=%v overrides the workload durations. "+
			"Short runs are for validating the harness, not for producing results.", *duration)
	}

	for _, st := range runOrder([]string{"null", *arm}, wls, *repeats) {
		w, _ := workload.ByName(st.Workload)
		if *duration > 0 {
			w.Duration = *duration
		}
		target := *proxyURL
		if st.Arm == "null" {
			target = *nullURL
		}
		log.Printf("running arm=%s workload=%s repeat=%d", st.Arm, st.Workload, st.Repeat)

		rec, err := runOne(st, w, target, newRemote(*remoteCmd, *proxyHost), *procName, *scriptDir,
			*forceCopy, spliceguard.Tolerances{MaxCallsPerRequest: *spliceMaxCalls, MinBytesPerSpliceFraction: *spliceMinBytesFrac})
		if err != nil {
			log.Fatalf("run failed (arm=%s workload=%s repeat=%d): %v",
				st.Arm, st.Workload, st.Repeat, err)
		}
		path, err := result.Write(*outDir, rec)
		if err != nil {
			log.Fatalf("writing result: %v", err)
		}
		log.Printf("  -> %s  rps=%.0f p99.9=%v", path, rec.RPS, rec.EndToEnd.P999)
	}
}

func runOne(st step, w workload.Workload, target string, rem remote, procName, scriptDir string,
	forceCopy bool, spliceTol spliceguard.Tolerances) (result.Run, error) {
	var rec result.Run

	// Proxy-side sampling is skipped for the null arm: there is no proxy in the
	// path, so there are no workers to sample and no counters to attribute.
	var before []float64
	var pids []int
	if st.Arm != "null" {
		var err error
		if pids, err = remotePIDs(rem, procName); err != nil {
			return rec, fmt.Errorf("resolving %s pids: %w", procName, err)
		}
		if before, err = remoteSample(rem, pids); err != nil {
			return rec, fmt.Errorf("sampling before: %w", err)
		}
	}

	argv := w.WrkArgs(target, scriptDir)
	env := append(os.Environ(),
		"RAILGUN_HEAVY_PATH="+w.Paths[0],
		"RAILGUN_LIGHT_PATH="+w.Paths[len(w.Paths)-1],
		"RAILGUN_HEAVY_THREADS="+strconv.Itoa(heavyThreads(w)),
	)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return rec, fmt.Errorf("wrk2: %w", err)
	}

	out, err := wrk.Parse(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return rec, fmt.Errorf("parsing wrk2 output: %w", err)
	}

	rec = result.Run{
		Schema:    result.Schema,
		RunID:     fmt.Sprintf("%s-%s-%s-%d", time.Now().UTC().Format("20060102-150405"), st.Arm, st.Workload, st.Repeat),
		Timestamp: time.Now().UTC(),
		Arm:       st.Arm,
		Workload:  st.Workload,
		Repeat:    st.Repeat,
		RPS:       out.RPS,
		Requests:  out.Requests,
		EndToEnd:  out.Latency,
		WrkArgv:   argv,
		Counters:  map[string]float64{},
	}

	if st.Arm != "null" {
		after, err := remoteSample(rem, pids)
		if err != nil {
			return rec, fmt.Errorf("sampling after: %w", err)
		}
		rec.PerWorker = make([]float64, len(after))
		for i := range after {
			rec.PerWorker[i] = after[i] - before[i]
			rec.CPUSeconds += rec.PerWorker[i]
		}
		rec.Notes = fmt.Sprintf("cpu_skew=%.3f", proc.CPUSkew(rec.PerWorker))
		if forceCopy {
			rec.Notes += "; force_copy=true"
		}
	}

	// W4 splice guard (spec §5.5): a non-zero splice count is not enough — the
	// kernel can silently shrink the relay pipe (fs.pipe-user-pages-soft) and
	// splice keeps "working" at a fraction of its intended throughput. Only
	// checked once the splice counter has actually been collected (perf
	// collection lands separately; a missing key, per perf.Parse's own
	// convention, means "not measured" rather than "measured zero") and only
	// on the splice-enabled arm — -force-copy is the control arm, where zero
	// splices is the correct outcome.
	if st.Workload == "W4" {
		if spliceCount, ok := rec.Counters["syscalls:sys_enter_splice"]; ok {
			bodyBytes, err := w4BodyBytes(w)
			if err != nil {
				return rec, err
			}
			if err := spliceguard.Check(spliceCount, rec.Requests, bodyBytes, forceCopy, spliceTol); err != nil {
				return rec, err
			}
		}
	}
	return rec, nil
}

// w4BodyBytes recovers the response body size W4 requests from its origin
// path (e.g. "/bytes/1048576"), so the splice guard can check observed
// syscalls against the size the workload actually asked for rather than a
// second hard-coded constant.
func w4BodyBytes(w workload.Workload) (int64, error) {
	p := w.Paths[len(w.Paths)-1]
	i := strings.LastIndex(p, "/")
	n, err := strconv.ParseInt(p[i+1:], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("workload %s: path %q does not encode a body size", w.Name, p)
	}
	return n, nil
}

// heavyThreads converts a workload's HeavyRatio into a whole number of wrk2
// threads. wrk2 assigns connections to threads, and heaviness must be sticky
// per connection for the H1 mechanism to exist at all (spec §5.1.1) — so the
// unit of assignment is the thread, not the request. At least one thread is
// heavy whenever the ratio is non-zero.
// preflight confirms a target answers HTTP before a long run commits to it.
func preflight(rawURL string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(strings.TrimRight(rawURL, "/") + "/bytes/16")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /bytes/16 returned %s", resp.Status)
	}
	return nil
}

func heavyThreads(w workload.Workload) int {
	if w.HeavyRatio <= 0 {
		return 0
	}
	n := int(float64(w.Threads)*w.HeavyRatio + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}

func remotePIDs(rem remote, name string) ([]int, error) {
	a := rem.argv("pgrep -x " + name)
	out, err := exec.Command(a[0], a[1:]...).Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("no processes named %q on the proxy host", name)
	}
	return pids, nil
}

// remoteSample reads /proc/<pid>/stat for each pid over one SSH invocation.
// Records are separated by a sentinel because a stat line contains no newline
// and the comm field can contain almost anything.
func remoteSample(rem remote, pids []int) ([]float64, error) {
	var b strings.Builder
	for _, pid := range pids {
		fmt.Fprintf(&b, "cat /proc/%d/stat; echo; echo '---'; ", pid)
	}
	a := rem.argv(b.String())
	out, err := exec.Command(a[0], a[1:]...).Output()
	if err != nil {
		return nil, err
	}
	var samples []float64
	for _, chunk := range strings.Split(string(out), "---") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		u, s, err := proc.ParseStat([]byte(chunk))
		if err != nil {
			return nil, err
		}
		samples = append(samples, float64(u+s)/100.0) // USER_HZ
	}
	if len(samples) != len(pids) {
		return nil, fmt.Errorf("sampled %d processes, expected %d", len(samples), len(pids))
	}
	return samples, nil
}

func cmdReport(argv []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	inDir := fs.String("in", "bench/results", "directory of run records")
	fs.Parse(argv)

	runs, err := result.LoadDir(*inDir)
	if err != nil {
		log.Fatalf("loading results: %v", err)
	}
	if err := report.Render(os.Stdout, agg.Summarize(runs)); err != nil {
		log.Fatalf("rendering report: %v", err)
	}
	_ = perf.Events // counters are wired in when perf collection lands on the rig
}
