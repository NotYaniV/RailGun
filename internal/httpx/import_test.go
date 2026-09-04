package httpx_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoIODependency enforces the invariant from design spec section 7.1:
// httpx must not depend on the I/O model.
//
// The spec sketches this with golang.org/x/tools/go/packages. Shelling out to
// `go list -deps` gets the same answer from the same build graph without
// adding a module dependency, and this repository currently has none.
//
// The invariant is not stylistic. Milestone M3 replaces the goroutine-per-
// connection data plane with an io_uring completion loop, and this package is
// meant to be reused there unchanged. A single transitive import of net or
// bufio would mean rewriting the parser under the ring instead of moving it.
func TestNoIODependency(t *testing.T) {
	banned := []string{
		"net",
		"bufio",
		"os",
		"golang.org/x/sys/unix",
		"syscall",
		"net/http",
	}

	out, err := exec.Command("go", "list", "-deps", "github.com/NotYaniV/RailGun/internal/httpx").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	deps := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps[line] = true
		}
	}
	if len(deps) == 0 {
		t.Fatal("go list returned no dependencies; the guard would pass vacuously")
	}

	for _, b := range banned {
		if deps[b] {
			t.Errorf("httpx transitively imports %q, violating the I/O-independence invariant (spec 7.1)", b)
		}
	}
}
