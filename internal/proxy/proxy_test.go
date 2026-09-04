package proxy_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/proxy"
)

// rig is a live origin plus a live proxy, wired over real sockets.
//
// The test deliberately does not stub the network. Every claim this project
// makes rests on splice, keepalive reuse and framing behaviour that only exist
// once real TCP connections are involved; a test against an in-memory pipe
// would pass while the proxy failed on a socket.
type rig struct {
	t      *testing.T
	origin *http.Server
	srv    *proxy.Server
	addr   string
}

func newRig(t *testing.T, opts proxy.Options, h http.Handler) *rig {
	t.Helper()

	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := &http.Server{Handler: h}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })

	snap, err := config.NewSnapshot(1,
		[]config.Route{{Host: "app.test", Upstream: "app"}},
		[]config.Upstream{{Name: "app", Addrs: []string{oln.Addr().String()}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var store config.Store
	store.Store(snap)

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(&store, opts)
	go srv.Serve([]*net.TCPListener{pln.(*net.TCPListener)})
	t.Cleanup(func() {
		done := make(chan struct{})
		close(done)
		pln.Close()
		srv.Shutdown(done)
	})

	return &rig{t: t, origin: origin, srv: srv, addr: pln.Addr().String()}
}

// do issues a request through the proxy with a stock net/http client. Using
// the standard client rather than hand-written bytes is the point: it holds
// the proxy to the framing rules a real client enforces.
func (r *rig) client() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 4,
			DisableCompression:  true,
		},
	}
}

func (r *rig) get(c *http.Client, path string) (*http.Response, []byte) {
	r.t.Helper()
	req, err := http.NewRequest("GET", "http://"+r.addr+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = "app.test"
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		r.t.Fatal(err)
	}
	return resp, body
}

func testOpts() proxy.Options {
	o := proxy.DefaultOptions()
	o.IdleTimeout = 2 * time.Second
	o.ReadTimeout = 2 * time.Second
	o.WriteTimeout = 2 * time.Second
	o.UpstreamTimeout = 5 * time.Second
	o.MaxUpstreamConns = 256
	return o
}

func echoHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/small", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin", "yes")
		w.Write([]byte("hello"))
	})
	mux.HandleFunc("/bytes/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/bytes/"), "%d", &n)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = byte('a' + i%26)
		}
		w.Header().Set("Content-Length", fmt.Sprint(n))
		w.Write(buf)
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "chunk%d\n", i)
			fl.Flush()
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Write(b)
	})
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/status/"), "%d", &n)
		w.WriteHeader(n)
	})
	return mux
}

func TestProxyBasicGET(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	resp, body := r.get(c, "/small")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-Origin") != "yes" {
		t.Error("origin header not forwarded")
	}
}

// TestProxyKeepAliveReuse is the test that proves the pool is doing its job.
// Ten sequential requests over one client connection must produce far fewer
// upstream dials than requests; if they do not, every request is paying a
// connect and the whole upstream pool is decorative.
func TestProxyKeepAliveReuse(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	for i := 0; i < 10; i++ {
		resp, body := r.get(c, "/small")
		if resp.StatusCode != 200 || string(body) != "hello" {
			t.Fatalf("request %d: status=%d body=%q", i, resp.StatusCode, body)
		}
	}

	var dials, reuses uint64
	for _, st := range r.srv.PoolStats() {
		dials += st.Dials
		reuses += st.Reuses
	}
	if reuses == 0 {
		t.Fatalf("no upstream connection was reused across 10 requests (dials=%d)", dials)
	}
	if dials >= 10 {
		t.Fatalf("dials=%d for 10 requests; the pool is not reusing connections", dials)
	}
	t.Logf("10 requests: %d dials, %d reuses", dials, reuses)
}

// TestProxyBodySizes walks the splice threshold from both sides. The proxy
// must return byte-identical bodies whichever path it took, which is the
// property that lets experiment E2 swap paths and compare only speed.
func TestProxyBodySizes(t *testing.T) {
	o := testOpts()
	o.SpliceThreshold = 8 << 10
	r := newRig(t, o, echoHandler())
	c := r.client()

	for _, n := range []int{0, 1, 1024, 8191, 8192, 8193, 64 << 10, 1 << 20} {
		resp, body := r.get(c, fmt.Sprintf("/bytes/%d", n))
		if resp.StatusCode != 200 {
			t.Fatalf("n=%d: status %d", n, resp.StatusCode)
		}
		if len(body) != n {
			t.Fatalf("n=%d: got %d bytes", n, len(body))
		}
		for i := range body {
			if body[i] != byte('a'+i%26) {
				t.Fatalf("n=%d: corruption at offset %d", n, i)
			}
		}
	}
}

// TestProxyForceCopyMatchesSplice is the correctness half of experiment E2.
// The two arms must be byte-identical; only their cost may differ.
func TestProxyForceCopyMatchesSplice(t *testing.T) {
	payload := "/bytes/1048576"

	o1 := testOpts()
	o1.SpliceThreshold = 1024
	spliced := newRig(t, o1, echoHandler())
	_, a := spliced.get(spliced.client(), payload)

	o2 := testOpts()
	o2.ForceCopy = true
	copied := newRig(t, o2, echoHandler())
	_, b := copied.get(copied.client(), payload)

	if len(a) != len(b) || !bytes.Equal(a, b) {
		t.Fatalf("splice arm and force-copy arm disagree: %d vs %d bytes", len(a), len(b))
	}
	if len(a) != 1<<20 {
		t.Fatalf("got %d bytes, want %d", len(a), 1<<20)
	}
}

func TestProxyChunkedResponse(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	resp, body := r.get(c, "/chunked")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := "chunk0\nchunk1\nchunk2\nchunk3\nchunk4\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}

	// The connection must still be usable: the chunk scanner has to have found
	// the terminal chunk exactly, or the next request desynchronizes.
	resp2, body2 := r.get(c, "/small")
	if resp2.StatusCode != 200 || string(body2) != "hello" {
		t.Fatalf("request after chunked response failed: %d %q", resp2.StatusCode, body2)
	}
}

func TestProxyRequestBody(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	for _, n := range []int{1, 4096, 1 << 20} {
		payload := make([]byte, n)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("POST", "http://"+r.addr+"/echo", bytes.NewReader(payload))
		req.Host = "app.test"
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("n=%d: echo mismatch (%d bytes back)", n, len(got))
		}
	}
}

func TestProxyChunkedRequestBody(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	payload := strings.Repeat("xyz", 5000)
	// A nil ContentLength with a non-seekable body makes net/http send chunked.
	req, _ := http.NewRequest("POST", "http://"+r.addr+"/echo", io.NopCloser(strings.NewReader(payload)))
	req.Host = "app.test"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != payload {
		t.Fatalf("chunked echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestProxyHEAD guards the framing rule that a HEAD response declares a
// Content-Length for a body it does not send. A proxy that waits for those
// bytes hangs until timeout and then poisons the pooled connection, so the
// real assertion is the SECOND request completing.
func TestProxyHEAD(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	req, _ := http.NewRequest("HEAD", "http://"+r.addr+"/bytes/4096", nil)
	req.Host = "app.test"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.ContentLength != 4096 {
		t.Errorf("ContentLength = %d, want 4096", resp.ContentLength)
	}

	resp2, body := r.get(c, "/small")
	if resp2.StatusCode != 200 || string(body) != "hello" {
		t.Fatal("connection unusable after HEAD; the proxy waited for a body that was never sent")
	}
}

func TestProxyBodylessStatuses(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())
	c := r.client()

	for _, code := range []int{204, 304, 404, 500} {
		resp, _ := r.get(c, fmt.Sprintf("/status/%d", code))
		if resp.StatusCode != code {
			t.Fatalf("got %d, want %d", resp.StatusCode, code)
		}
	}
	resp, body := r.get(c, "/small")
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Fatal("connection unusable after bodyless statuses")
	}
}

func TestProxyUnknownHostIs502(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	req, _ := http.NewRequest("GET", "http://"+r.addr+"/small", nil)
	req.Host = "nowhere.test"
	resp, err := r.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestProxyRejectsSmuggling checks that the framing defenses in internal/httpx
// are actually reachable through the running proxy, not merely unit-tested.
// A raw connection is used because no correct client will send these.
func TestProxyRejectsSmuggling(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "CL and TE together",
			raw:  "POST /echo HTTP/1.1\r\nHost: app.test\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			want: "400",
		},
		{
			name: "duplicate Content-Length",
			raw:  "POST /echo HTTP/1.1\r\nHost: app.test\r\nContent-Length: 6\r\nContent-Length: 5\r\n\r\n",
			want: "400",
		},
		{
			name: "space before colon",
			raw:  "GET /small HTTP/1.1\r\nHost: app.test\r\nContent-Length : 0\r\n\r\n",
			want: "400",
		},
		{
			name: "obs-fold",
			raw:  "GET /small HTTP/1.1\r\nHost: app.test\r\nX-A: 1\r\n 2\r\n\r\n",
			want: "400",
		},
		{
			name: "bare LF framing",
			raw:  "GET /small HTTP/1.1\r\nHost: app.test\nX-A: 1\r\n\r\n",
			want: "400",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := c.Write([]byte(tc.raw)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 256)
			n, _ := c.Read(buf)
			got := string(buf[:n])
			if n == 0 {
				t.Fatal("proxy sent no response and did not reject the request")
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("response %q does not contain %q", got, tc.want)
			}
		})
	}
}

// TestProxyOversizedHeadIs431 checks the trust-boundary limit from spec 5.3:
// an oversized header block is rejected, never grown into.
func TestProxyOversizedHeadIs431(t *testing.T) {
	o := testOpts()
	o.ReadBufferSize = 2048
	r := newRig(t, o, echoHandler())

	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))

	var b strings.Builder
	b.WriteString("GET /small HTTP/1.1\r\nHost: app.test\r\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "X-Pad-%03d: %s\r\n", i, strings.Repeat("v", 64))
	}
	b.WriteString("\r\n")
	c.Write([]byte(b.String()))

	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "431") {
		t.Fatalf("response %q, want 431", got)
	}
}

func TestProxyConcurrent(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	const clients, each = 16, 20
	errc := make(chan error, clients)
	for i := 0; i < clients; i++ {
		go func() {
			c := r.client()
			for j := 0; j < each; j++ {
				req, _ := http.NewRequest("GET", "http://"+r.addr+"/bytes/4096", nil)
				req.Host = "app.test"
				resp, err := c.Do(req)
				if err != nil {
					errc <- err
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if len(b) != 4096 {
					errc <- fmt.Errorf("got %d bytes, want 4096", len(b))
					return
				}
			}
			errc <- nil
		}()
	}
	for i := 0; i < clients; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	if inUse := r.srv.UpstreamConnsInUse(); inUse > clients {
		t.Errorf("slab holds %d connections after %d clients; suspect a leak", inUse, clients)
	}
}
