// Command stress is a portable stress/robustness suite for a running proxy.
//
// It is deliberately not a replacement for the M0 rig (wrk2 + bench runner):
// it answers "does it fall over, mis-frame, leak or stall" rather than "is it
// faster than NGINX". Same scenarios on any OS, results written as JSON so a
// macOS run and a Linux run (or Railgun and NGINX) can be diffed.
//
//	./bin/stress -proxy 127.0.0.1:8080 -admin 127.0.0.1:9090 -pid $(pgrep railgun) -out run.json
//
// Needs an origin that serves GET /bytes/N (bin/origin). -admin and -pid are
// optional; scenarios that need them say so and skip.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	proxy  = flag.String("proxy", "127.0.0.1:8080", "proxy host:port")
	admin  = flag.String("admin", "", "proxy admin host:port for /metrics (optional)")
	host   = flag.String("host", "app.test", "Host header the proxy routes on")
	pid    = flag.Int("pid", 0, "proxy pid, for RSS sampling (optional)")
	out    = flag.String("out", "", "write JSON results here")
	only   = flag.String("only", "", "comma-separated scenario names; empty = all")
	dur    = flag.Duration("d", 6*time.Second, "duration of each load step")
	quick  = flag.Bool("quick", false, "short steps, smaller counts (smoke test)")
	listen = flag.Bool("list", false, "list scenarios and exit")
)

// Result is one row. Verdict is PASS/FAIL where the criterion is objective
// (framing, errors, leaks) and INFO where it is a measurement to compare.
type Result struct {
	Scenario string         `json:"scenario"`
	Case     string         `json:"case"`
	Verdict  string         `json:"verdict"`
	Metrics  map[string]any `json:"metrics"`
	Note     string         `json:"note,omitempty"`
}

var results []Result

func record(sc, c, verdict, note string, kv ...any) {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	results = append(results, Result{sc, c, verdict, m, note})
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%v", kv[i], kv[i+1]))
	}
	fmt.Printf("%-9s %-34s %-4s %s %s\n", sc, c, verdict, strings.Join(parts, " "), note)
}

func step() time.Duration {
	if *quick {
		return 2 * time.Second
	}
	return *dur
}
func scale(n int) int {
	if *quick {
		return max(n/10, 5)
	}
	return n
}

// ---- client plumbing ------------------------------------------------------

func get(path string, extra string) string {
	return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", path, *host, extra)
}

// readResp reads one Content-Length-framed response and returns status code,
// body length, and whether the server said Connection: close.
func readResp(br *bufio.Reader) (code, n int, closing bool, err error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, 0, false, err
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0, 0, false, fmt.Errorf("bad status line %q", line)
	}
	code, _ = strconv.Atoi(f[1])
	cl := -1
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return code, 0, false, err
		}
		if h == "\r\n" {
			break
		}
		l := strings.ToLower(h)
		if strings.HasPrefix(l, "content-length:") {
			cl, _ = strconv.Atoi(strings.TrimSpace(l[15:]))
		}
		if strings.HasPrefix(l, "connection:") && strings.Contains(l, "close") {
			closing = true
		}
	}
	if cl < 0 {
		return code, 0, closing, fmt.Errorf("no content-length")
	}
	got, err := io.CopyN(io.Discard, br, int64(cl))
	return code, int(got), closing, err
}

func dial() (net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", *proxy, 3*time.Second)
	if err != nil {
		return nil, nil, err
	}
	return c, bufio.NewReaderSize(c, 64<<10), nil
}

func pct(l []time.Duration, p float64) string {
	if len(l) == 0 {
		return "-"
	}
	return l[int(float64(len(l)-1)*p)].Round(time.Microsecond).String()
}

type load struct {
	path   string
	conns  int
	rate   int // total rps, 0 = closed loop
	churn  bool
	d      time.Duration
	expect int // expected body length, -1 = don't check
}

type loadOut struct {
	ok, non200, errs, short int64
	lats                    []time.Duration
	rps                     float64
}

func (o loadOut) kv() []any {
	return []any{"rps", int(o.rps), "p50", pct(o.lats, .5), "p99", pct(o.lats, .99),
		"p999", pct(o.lats, .999), "max", pct(o.lats, 1), "non200", o.non200, "errs", o.errs, "short", o.short}
}

func runLoad(l load) loadOut {
	var ok, bad, errs, short atomic.Int64
	var mu sync.Mutex
	var all []time.Duration
	var wg sync.WaitGroup
	end := time.Now().Add(l.d)
	extra := ""
	if l.churn {
		extra = "Connection: close\r\n"
	}
	req := get(l.path, extra)
	for i := 0; i < l.conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var c net.Conn
			var br *bufio.Reader
			var local []time.Duration
			var interval time.Duration
			if l.rate > 0 {
				interval = time.Duration(float64(time.Second) * float64(l.conns) / float64(l.rate))
			}
			next := time.Now()
			for time.Now().Before(end) {
				if c == nil {
					var err error
					if c, br, err = dial(); err != nil {
						errs.Add(1)
						time.Sleep(5 * time.Millisecond)
						continue
					}
				}
				start := time.Now()
				if interval > 0 { // open loop: latency from intended send time, no coordinated omission
					if d := time.Until(next); d > 0 {
						time.Sleep(d)
					}
					start, next = next, next.Add(interval)
				}
				c.SetDeadline(time.Now().Add(10 * time.Second))
				_, err := io.WriteString(c, req)
				var code, n int
				var closing bool
				if err == nil {
					code, n, closing, err = readResp(br)
				}
				if err != nil {
					errs.Add(1)
					c.Close()
					c = nil
					continue
				}
				if code == 200 {
					ok.Add(1)
				} else {
					bad.Add(1)
				}
				if l.expect >= 0 && code == 200 && n != l.expect {
					short.Add(1)
				}
				local = append(local, time.Since(start))
				if l.churn || closing {
					c.Close()
					c = nil
				}
			}
			if c != nil {
				c.Close()
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return loadOut{ok.Load(), bad.Load(), errs.Load(), short.Load(), all, float64(len(all)) / l.d.Seconds()}
}

// ---- admin/RSS ------------------------------------------------------------

func metrics() map[string]float64 {
	if *admin == "" {
		return nil
	}
	c, err := net.DialTimeout("tcp", *admin, 2*time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "GET /metrics HTTP/1.0\r\n\r\n")
	b, _ := io.ReadAll(c)
	m := map[string]float64{}
	for _, ln := range strings.Split(string(b), "\n") {
		if i := strings.LastIndexByte(ln, ' '); i > 0 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(ln[i+1:]), 64); err == nil {
				m[ln[:i]] = v
			}
		}
	}
	return m
}

func metric(m map[string]float64, prefix string) float64 {
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			return v
		}
	}
	return -1
}

func rssMB() float64 {
	if *pid == 0 {
		return -1
	}
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(*pid)).Output()
	if err != nil {
		return -1
	}
	k, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return float64(k) / 1024
}

// probe returns the latency of one normal request, or -1 on failure.
func probe() time.Duration {
	c, br, err := dial()
	if err != nil {
		return -1
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	t := time.Now()
	io.WriteString(c, get("/bytes/100", "Connection: close\r\n"))
	if code, _, _, err := readResp(br); err != nil || code != 200 {
		return -1
	}
	return time.Since(t)
}

// probeWhile samples probe() once a second until stop closes; returns
// failures and worst latency.
func probeWhile(stop <-chan struct{}) (fails int, worst time.Duration) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
			if d := probe(); d < 0 {
				fails++
			} else if d > worst {
				worst = d
			}
		}
	}
}

// ---- scenarios ------------------------------------------------------------

func sConc() {
	for _, c := range []int{1, 16, 64, 128, 256, 1024} {
		o := runLoad(load{"/bytes/100", c, 0, false, step(), 100})
		v := "INFO"
		if o.errs > 0 || o.non200 > 0 || o.short > 0 {
			v = "FAIL"
		}
		record("conc", fmt.Sprintf("c=%d", c), v, "", o.kv()...)
		time.Sleep(time.Second)
	}
}

func sSizes() {
	for _, n := range []int{0, 1024, 32767, 32768, 65536, 1 << 20} {
		o := runLoad(load{"/bytes/" + strconv.Itoa(n), 64, 0, false, step(), n})
		v := "INFO"
		if o.errs > 0 || o.non200 > 0 || o.short > 0 {
			v = "FAIL"
		}
		record("sizes", fmt.Sprintf("bytes=%d c=64", n), v, "", o.kv()...)
	}
}

func sRate() {
	for _, r := range []int{5000, 20000, 40000} {
		o := runLoad(load{"/bytes/100", 32, r, false, step(), 100})
		v := "INFO"
		if o.errs > 0 || o.non200 > 0 {
			v = "FAIL"
		}
		record("rate", fmt.Sprintf("open-loop %d rps", r), v, "", o.kv()...)
	}
}

func sChurn() {
	o := runLoad(load{"/bytes/100", 32, 0, true, step(), 100})
	v := "INFO"
	if o.errs > 0 || o.non200 > 0 {
		v = "FAIL"
	}
	record("churn", "conn-per-request c=32", v, "", o.kv()...)
}

// sPipeline writes bursts of N requests back-to-back with distinct sizes and
// verifies every response arrives, in order, with the right length.
func sPipeline() {
	sizes := []int{0, 1, 100, 32767, 32768, 32769, 70000}
	for _, burst := range []int{10, 100} {
		var bad, total atomic.Int64
		var wg sync.WaitGroup
		end := time.Now().Add(step())
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(end) {
					c, br, err := dial()
					if err != nil {
						bad.Add(1)
						continue
					}
					c.SetDeadline(time.Now().Add(15 * time.Second))
					var b bytes.Buffer
					for i := 0; i < burst; i++ {
						b.WriteString(get("/bytes/"+strconv.Itoa(sizes[i%len(sizes)]), ""))
					}
					go c.Write(b.Bytes())
					for i := 0; i < burst; i++ {
						code, n, _, err := readResp(br)
						total.Add(1)
						if err != nil || code != 200 || n != sizes[i%len(sizes)] {
							bad.Add(1)
							break
						}
					}
					c.Close()
				}
			}()
		}
		wg.Wait()
		v := "PASS"
		if bad.Load() > 0 {
			v = "FAIL"
		}
		record("pipeline", fmt.Sprintf("burst=%d ordered+verified", burst), v, "", "responses", total.Load(), "bad", bad.Load())
	}
}

// sMixed cycles sizes around the splice threshold on ONE connection per
// worker, which is where framing drift between messages would show.
func sMixed() {
	sizes := []int{0, 1, 100, 32767, 32768, 32769, 1 << 20}
	var bad, total atomic.Int64
	var wg sync.WaitGroup
	end := time.Now().Add(step())
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, br, err := dial()
			if err != nil {
				bad.Add(1)
				return
			}
			defer c.Close()
			for i := 0; time.Now().Before(end); i++ {
				n := sizes[i%len(sizes)]
				c.SetDeadline(time.Now().Add(10 * time.Second))
				io.WriteString(c, get("/bytes/"+strconv.Itoa(n), ""))
				code, got, _, err := readResp(br)
				total.Add(1)
				if err != nil || code != 200 || got != n {
					bad.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	v := "PASS"
	if bad.Load() > 0 {
		v = "FAIL"
	}
	record("mixed", "sizes around splice threshold, keepalive", v, "", "responses", total.Load(), "bad", bad.Load())
}

// sAbort: clients that RST mid-response. Then check nothing leaked.
func sAbort() {
	n := scale(3000)
	before, rss0 := metrics(), rssMB()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			c, err := net.DialTimeout("tcp", *proxy, 3*time.Second)
			if err != nil {
				return
			}
			io.WriteString(c, get("/bytes/1048576", ""))
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			io.ReadFull(c, make([]byte, 10))
			c.(*net.TCPConn).SetLinger(0) // close sends RST
			c.Close()
		}()
	}
	wg.Wait()
	time.Sleep(2 * time.Second)
	after, rss1 := metrics(), rssMB()
	note := ""
	v := "PASS"
	if probe() < 0 {
		v, note = "FAIL", "proxy unresponsive after aborts"
	}
	kv := []any{"aborts", n, "rss_before_mb", int(rss0), "rss_after_mb", int(rss1)}
	if before != nil {
		kv = append(kv, "pool_idle_after", metric(after, "railgun_pool_idle"))
		// in_use counts pooled idle conns too; a leak shows as it exceeding idle.
		if metric(after, "railgun_upstream_conns_in_use") > metric(after, "railgun_pool_idle")+2 {
			v, note = "FAIL", "upstream conns in use exceed idle after clients left: leak"
		}
	}
	record("abort", "RST mid 1MiB response", v, note, kv...)
}

func sSoak() {
	d := 30 * time.Second
	if *quick {
		d = 6 * time.Second
	}
	var samples []int
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(d / 6):
				samples = append(samples, int(rssMB()))
			}
		}
	}()
	o := runLoad(load{"/bytes/1024", 32, 20000, false, d, 1024})
	close(stop)
	v := "INFO"
	if o.errs > 0 || o.non200 > 0 || o.short > 0 {
		v = "FAIL"
	}
	record("soak", fmt.Sprintf("%s @20k rps", d), v, "", append(o.kv(), "rss_mb_series", fmt.Sprint(samples))...)
}

// held opens n connections and runs hold on each; reports how many the proxy
// closed within the window while a normal client is probed for starvation.
func sHeld(name string, n int, window time.Duration, hold func(c net.Conn, stop <-chan struct{})) {
	rss0 := rssMB()
	var closed, opened atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", *proxy, 3*time.Second)
		if err != nil {
			continue
		}
		opened.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			hold(c, stop)
			select {
			case <-stop:
			default:
				closed.Add(1)
			}
		}()
	}
	pstop := make(chan struct{})
	var fails int
	var worst time.Duration
	pdone := make(chan struct{})
	go func() { fails, worst = probeWhile(pstop); close(pdone) }()
	time.Sleep(window)
	close(pstop)
	<-pdone
	close(stop)
	wg.Wait()
	v, note := "PASS", ""
	if fails > 0 {
		v, note = "FAIL", "normal clients failed while hostile conns held"
	}
	record(name, fmt.Sprintf("%d hostile conns, %s window", n, window), v, note,
		"opened", opened.Load(), "closed_by_proxy", closed.Load(), "probe_fails", fails,
		"probe_worst", worst.Round(time.Microsecond).String(), "rss_before_mb", int(rss0), "rss_after_mb", int(rssMB()))
}

func sSlowloris() {
	w := 20 * time.Second
	if *quick {
		w = 5 * time.Second
	}
	sHeld("slowloris", scale(500), w, func(c net.Conn, stop <-chan struct{}) {
		io.WriteString(c, "GET /bytes/1 HTTP/1.1\r\nHost: "+*host+"\r\nX-A: ")
		go func() { io.Copy(io.Discard, c) }() // notice when the proxy closes us
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				if _, err := io.WriteString(c, "a"); err != nil {
					return
				}
			}
		}
	})
}

func sIdle() {
	w := 20 * time.Second
	if *quick {
		w = 5 * time.Second
	}
	sHeld("idle", scale(200), w, func(c net.Conn, stop <-chan struct{}) {
		c.SetReadDeadline(time.Now().Add(w + 2*time.Second))
		io.Copy(io.Discard, c)
	})
}

func sSlowRead() {
	w := 15 * time.Second
	if *quick {
		w = 5 * time.Second
	}
	sHeld("slowread", scale(100), w, func(c net.Conn, stop <-chan struct{}) {
		io.WriteString(c, get("/bytes/1048576", ""))
		b := make([]byte, 1)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				if _, err := c.Read(b); err != nil {
					return
				}
			}
		}
	})
}

type bad struct {
	name    string
	payload string
}

// sMalformed sends hostile requests. Every one of these must be refused (4xx
// or close, never 2xx), and after a refusal the connection must not go on to
// answer a smuggled follow-up. The final case is a control: two well-formed
// pipelined requests must both be answered.
func sMalformed() {
	h := "Host: " + *host + "\r\n"
	cases := []bad{
		{"CL+TE conflicting framing", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"CL.TE smuggle", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nG"},
		{"TE.CL smuggle", "GET /bytes/5 HTTP/1.1\r\n" + h + "Transfer-Encoding: chunked\r\nContent-Length: 4\r\n\r\n5c\r\nGPOST / HTTP/1.1\r\n\r\n0\r\n\r\n"},
		{"duplicate differing Content-Length", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: 5\r\nContent-Length: 6\r\n\r\nhello!"},
		{"negative Content-Length", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: -1\r\n\r\n"},
		{"huge Content-Length", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: 99999999999999999999\r\n\r\n"},
		{"non-numeric Content-Length", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length: 5abc\r\n\r\nhello"},
		{"bad chunk size", "GET /bytes/5 HTTP/1.1\r\n" + h + "Transfer-Encoding: chunked\r\n\r\nZZ\r\nhello\r\n0\r\n\r\n"},
		{"obfuscated TE (xchunked)", "GET /bytes/5 HTTP/1.1\r\n" + h + "Transfer-Encoding: xchunked\r\nContent-Length: 5\r\n\r\nhello"},
		{"space before header colon", "GET /bytes/5 HTTP/1.1\r\n" + h + "Content-Length : 5\r\n\r\nhello"},
		{"obs-fold header", "GET /bytes/5 HTTP/1.1\r\n" + h + "X-A: b\r\n c\r\n\r\n"},
		{"bare LF line endings", "GET /bytes/5 HTTP/1.1\nHost: " + *host + "\n\n"},
		{"NUL in header value", "GET /bytes/5 HTTP/1.1\r\n" + h + "X-A: a\x00b\r\n\r\n"},
		{"NUL in path", "GET /bytes/5\x00 HTTP/1.1\r\n" + h + "\r\n"},
		{"garbage request line", "FOO\r\n\r\n"},
		{"HTTP/0.9", "GET /bytes/5\r\n\r\n"},
		{"HTTP/2 preface", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"},
		{"no Host header", "GET /bytes/5 HTTP/1.1\r\n\r\n"},
		{"1MiB header line", "GET /bytes/5 HTTP/1.1\r\n" + h + "X-A: " + strings.Repeat("a", 1<<20) + "\r\n\r\n"},
		{"10000 headers", "GET /bytes/5 HTTP/1.1\r\n" + h + strings.Repeat("X-A: b\r\n", 10000) + "\r\n"},
		{"64KiB URL", "GET /" + strings.Repeat("a", 64<<10) + " HTTP/1.1\r\n" + h + "\r\n"},
	}
	follow := get("/bytes/5", "Connection: close\r\n")
	for _, tc := range cases {
		c, br, err := dial()
		if err != nil {
			record("malformed", tc.name, "FAIL", "dial failed", "err", err.Error())
			continue
		}
		c.SetDeadline(time.Now().Add(4 * time.Second))
		go c.Write([]byte(tc.payload))
		code, _, _, rerr := readResp(br)
		// Did the connection survive to answer a follow-up?
		io.WriteString(c, follow)
		fcode, _, _, ferr := readResp(br)
		c.Close()
		v, note := "PASS", ""
		switch {
		case code >= 200 && code < 400:
			v, note = "FAIL", "hostile request was served"
		case fcode == 200 && ferr == nil:
			v, note = "FAIL", "connection stayed usable after refusal (desync risk)"
		}
		first := "closed/none"
		if rerr == nil || code != 0 {
			first = strconv.Itoa(code)
		}
		record("malformed", tc.name, v, note, "response", first, "followup_served", fcode == 200 && ferr == nil)
	}
	// control
	c, br, err := dial()
	if err == nil {
		c.SetDeadline(time.Now().Add(4 * time.Second))
		io.WriteString(c, get("/bytes/5", "")+get("/bytes/7", "Connection: close\r\n"))
		c1, n1, _, e1 := readResp(br)
		c2, n2, _, e2 := readResp(br)
		c.Close()
		v := "PASS"
		if e1 != nil || e2 != nil || c1 != 200 || c2 != 200 || n1 != 5 || n2 != 7 {
			v = "FAIL"
		}
		record("malformed", "CONTROL two valid pipelined", v, "")
	}
	if p := probe(); p < 0 {
		record("malformed", "proxy alive afterwards", "FAIL", "")
	} else {
		record("malformed", "proxy alive afterwards", "PASS", "")
	}
}

// Order matters: correctness first, then steady load, and the scenarios that
// burn ephemeral ports (churn, conc up to c=1024) last. On macOS in particular
// a collapse leaves tens of thousands of TIME_WAIT sockets and every later
// dial fails with "can't assign requested address", which would be blamed on
// the proxy.
var scenarios = []struct {
	name string
	fn   func()
}{
	{"malformed", sMalformed}, {"pipeline", sPipeline}, {"mixed", sMixed}, {"abort", sAbort},
	{"sizes", sSizes}, {"rate", sRate}, {"soak", sSoak},
	{"slowloris", sSlowloris}, {"idle", sIdle}, {"slowread", sSlowRead},
	{"churn", sChurn}, {"conc", sConc},
}

func main() {
	flag.Parse()
	if *listen {
		for _, s := range scenarios {
			fmt.Println(s.name)
		}
		return
	}
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s != "" {
			want[s] = true
		}
	}
	if probe() < 0 {
		fmt.Fprintf(os.Stderr, "stress: proxy at %s (Host: %s) not answering GET /bytes/100 with 200\n", *proxy, *host)
		os.Exit(2)
	}
	kernel, _ := exec.Command("uname", "-sr").Output()
	env := map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "ncpu": runtime.NumCPU(),
		"kernel": strings.TrimSpace(string(kernel)), "proxy": *proxy, "quick": *quick, "when": time.Now().Format(time.RFC3339)}
	fmt.Printf("env: %v\n", env)
	for _, s := range scenarios {
		if len(want) == 0 || want[s.name] {
			s.fn()
			time.Sleep(3 * time.Second) // let sockets drain between scenarios
		}
	}
	fail := 0
	for _, r := range results {
		if r.Verdict == "FAIL" {
			fail++
		}
	}
	fmt.Printf("\n%d results, %d FAIL\n", len(results), fail)
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{"env": env, "results": results}, "", "  ")
		os.WriteFile(*out, b, 0o644)
	}
	if fail > 0 {
		os.Exit(1)
	}
}
