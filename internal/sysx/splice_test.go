package sysx

import (
	"runtime"
	"strings"
	"testing"
)

func TestProbeSpliceReportsPlatformTruthfully(t *testing.T) {
	p := ProbeSplice(8)

	if p.PipesProbed != 8 {
		t.Fatalf("PipesProbed = %d, want 8", p.PipesProbed)
	}
	if p.WantPipeSize != SpliceWantPipeSize {
		t.Fatalf("WantPipeSize = %d, want %d", p.WantPipeSize, SpliceWantPipeSize)
	}

	switch runtime.GOOS {
	case "linux":
		// Available may legitimately be false under a seccomp policy that
		// denies splice, which is exactly the condition worth reporting. What
		// must always hold is that an unavailable result explains itself and an
		// available one has measured a real pipe size.
		if p.Available {
			if p.FirstSize <= 0 || p.SmallestSize <= 0 {
				t.Fatalf("available but no pipe size measured: %+v", p)
			}
			if p.PipesAtFull > p.PipesProbed {
				t.Fatalf("PipesAtFull %d exceeds PipesProbed %d", p.PipesAtFull, p.PipesProbed)
			}
		} else if p.Reason == "" {
			t.Fatal("unavailable on linux with no Reason — the operator learns nothing")
		}
	default:
		if p.Available {
			t.Fatalf("%s reports splice available; it is a Linux syscall", runtime.GOOS)
		}
		if !strings.Contains(p.Reason, "Linux") {
			t.Fatalf("Reason = %q, want it to name the platform limitation", p.Reason)
		}
	}
}

// TestProbeSpliceReport prints what this host actually grants. It asserts
// nothing beyond internal consistency — its value is the log line, which is the
// same line the proxy prints at startup. Run with -v on the rig before a
// session; a DEGRADED line there invalidates the splice arm before it costs a
// run.
func TestProbeSpliceReport(t *testing.T) {
	for _, n := range []int{1, 8, 64, 128} {
		p := ProbeSplice(n)
		t.Logf("pipes=%-4d %s", n, p)
		if p.Available && p.SmallestSize > p.FirstSize {
			t.Errorf("SmallestSize %d exceeds FirstSize %d", p.SmallestSize, p.FirstSize)
		}
	}
}

func TestProbeSpliceClampsPipeCount(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := ProbeSplice(n).PipesProbed; got != 1 {
			t.Fatalf("ProbeSplice(%d).PipesProbed = %d, want 1", n, got)
		}
	}
}

func TestSpliceProbeStringIsActionable(t *testing.T) {
	// The unavailable line must say what happens instead, not just "no".
	un := SpliceProbe{Reason: "splice(2) is Linux-only", WantPipeSize: SpliceWantPipeSize, PipesProbed: 1}
	if s := un.String(); !strings.Contains(s, "unavailable") || !strings.Contains(s, "userspace") {
		t.Fatalf("unavailable line = %q", s)
	}

	// The degraded line must name the limits, because naming them is the only
	// thing that lets an operator fix it before the run rather than after.
	deg := SpliceProbe{
		Available: true, WantPipeSize: SpliceWantPipeSize, PipesProbed: 64, PipesAtFull: 3,
		FirstSize: SpliceWantPipeSize, SmallestSize: 4096, MaxSize: 1048576, SoftPages: 16384,
	}
	if !deg.Degraded() {
		t.Fatal("3 of 64 pipes at full size is not being reported as degraded")
	}
	s := deg.String()
	for _, want := range []string{"DEGRADED", "4KiB", "3/64", "pipe-user-pages-soft=16384", "RUNBOOK"} {
		if !strings.Contains(s, want) {
			t.Fatalf("degraded line %q is missing %q", s, want)
		}
	}

	// A healthy probe must not shout.
	ok := SpliceProbe{
		Available: true, WantPipeSize: SpliceWantPipeSize, PipesProbed: 64, PipesAtFull: 64,
		FirstSize: SpliceWantPipeSize, SmallestSize: SpliceWantPipeSize,
	}
	if ok.Degraded() {
		t.Fatal("64/64 pipes at full size reported as degraded")
	}
	if s := ok.String(); !strings.Contains(s, "available") || strings.Contains(s, "DEGRADED") {
		t.Fatalf("healthy line = %q", s)
	}
}

func TestSpliceProbeUnknownSysctlsRenderAsUnknown(t *testing.T) {
	p := SpliceProbe{
		Available: true, WantPipeSize: SpliceWantPipeSize, PipesProbed: 2, PipesAtFull: 0,
		SmallestSize: 65536, MaxSize: -1, SoftPages: -1,
	}
	if s := p.String(); !strings.Contains(s, "fs.pipe-max-size=?") {
		t.Fatalf("unreadable sysctl should render as ?, got %q", s)
	}
}
