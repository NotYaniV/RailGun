// Package origin implements the upstream server that the proxy under test
// forwards to. It exists to make response size and upstream latency exactly
// controllable, so a workload's CPU cost is a property of the workload and not
// of an incidental backend.
package origin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBody caps a single response. Workloads top out at 1 MiB (spec §4.4); the
// cap exists so a malformed request cannot ask the rig to allocate unbounded
// memory mid-run and corrupt a measurement.
const maxBody = 8 << 20

type Config struct {
	// Delay is added before the response is written, simulating a slow
	// upstream. Note that per spec §5.1.1 this does NOT stress NGINX; it is
	// retained for control arms and sanity checks, not as W2's mechanism.
	Delay time.Duration
}

// filler is a single reusable page of body bytes. Responses are written by
// repeating it, so the origin never allocates per request and cannot itself
// become the bottleneck being measured.
var filler = func() []byte {
	b := make([]byte, 64<<10)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return b
}()

func Handler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/bytes/", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/bytes/"))
		if err != nil || n < 0 || n > maxBody {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
		if cfg.Delay > 0 {
			time.Sleep(cfg.Delay)
		}
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		for n > 0 {
			chunk := len(filler)
			if chunk > n {
				chunk = n
			}
			if _, err := w.Write(filler[:chunk]); err != nil {
				return
			}
			n -= chunk
		}
	})
	return mux
}
