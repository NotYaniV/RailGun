package origin

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestServesExactByteCount(t *testing.T) {
	h := Handler(Config{})
	for _, n := range []int{1024, 65536, 1048576} {
		req := httptest.NewRequest(http.MethodGet, "/bytes/"+itoa(n), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("n=%d: status = %d, want 200", n, rec.Code)
		}
		if got := rec.Body.Len(); got != n {
			t.Errorf("n=%d: body length = %d, want %d", n, got, n)
		}
		if got := rec.Header().Get("Content-Length"); got != itoa(n) {
			t.Errorf("n=%d: Content-Length = %q, want %q", n, got, itoa(n))
		}
	}
}

func TestRejectsBadSize(t *testing.T) {
	h := Handler(Config{})
	for _, path := range []string{"/bytes/abc", "/bytes/-1", "/bytes/", "/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("path %q: got 200, want an error status", path)
		}
	}
}

func TestInjectsDelay(t *testing.T) {
	h := Handler(Config{Delay: 40 * time.Millisecond})
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bytes/16", nil))
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 40ms", elapsed)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
