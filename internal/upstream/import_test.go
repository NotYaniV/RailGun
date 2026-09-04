package upstream_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoIODependency enforces the invariant from design spec section 7.1 on
// the upstream pool: it must not depend on the I/O model.
//
// The pool needs to open connections, and opening connections needs net. So
// dialing is not the pool's job: it stores opaque integer tokens and receives
// injected Dial and Close functions from its caller. Under Approach A that
// token indexes a slab of *net.TCPConn held by internal/proxy; under the
// io_uring data plane in M3 the same token becomes a registered-file index and
// the same pool code is reused without modification. That reuse is the whole
// point, and it survives only if this test keeps passing.
//
// syscall is deliberately NOT on the deny-list, and that exception is worth
// stating rather than leaving as an oversight. It arrives transitively through
// time, which the pool needs for idle expiry and dial backoff. Banning it would
// fail every package in the repository that tells the time, which would make
// the guard useless rather than strict. The packages that actually encode an
// I/O model — net, bufio, os, and golang.org/x/sys/unix — are the ones that
// matter here, and none of them is reachable.
//
// os in particular is easy to acquire by accident: fmt imports it, so a single
// fmt.Errorf anywhere in the package would fail this test. That is intended.
// The package hand-rolls its error wrapping for exactly this reason.
func TestNoIODependency(t *testing.T) {
	banned := []string{
		"net",
		"bufio",
		"os",
		"net/http",
		"golang.org/x/sys/unix",
	}

	out, err := exec.Command("go", "list", "-deps", "github.com/NotYaniV/RailGun/internal/upstream").Output()
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
			t.Errorf("upstream transitively imports %q, violating the I/O-independence invariant (spec 7.1)", b)
		}
	}
}
