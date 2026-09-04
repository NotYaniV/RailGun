package sysx

import "strconv"

// SpliceWantPipeSize is the pipe buffer size Go's internal/poll asks for on
// every splice pipe (its maxSpliceSize). It is duplicated here rather than
// imported because internal/poll is not importable, and it is the number that
// makes a probe meaningful: a granted size below this one means splice still
// works but moves this much less per call.
const SpliceWantPipeSize = 1 << 20

// SpliceProbe is what the kernel actually granted, as opposed to what Go asked
// for. It exists because the failure it detects is silent: Go's newPipe calls
// fcntl(F_SETPIPE_SZ) and discards the error, so a refused request costs
// throughput with nothing logged anywhere.
//
// See bench/RUNBOOK.md §5 for the two limits that refuse it.
type SpliceProbe struct {
	// Available reports whether splice(2) actually moved bytes here. It is a
	// functional check, not an inference from GOOS: seccomp profiles and some
	// sandboxes deny the syscall on kernels that otherwise support it.
	Available bool

	// Reason explains an Available=false, or a degraded result.
	Reason string

	WantPipeSize int // SpliceWantPipeSize, for reporting without a second constant
	PipesProbed  int // how many pipes were held open at once
	PipesAtFull  int // of those, how many were granted WantPipeSize
	FirstSize    int // bytes granted to the first pipe
	SmallestSize int // bytes granted to the least fortunate pipe

	// MaxSize is fs.pipe-max-size and SoftPages is fs.pipe-user-pages-soft,
	// both -1 when unreadable. They turn "you got 4 KiB" into "you got 4 KiB
	// because of this limit".
	MaxSize   int
	SoftPages int
}

// Degraded reports whether splice works but will move less per call than Go
// intends. This is the condition that costs throughput without failing
// anything, which makes it worth a loud startup line.
func (p SpliceProbe) Degraded() bool {
	return p.Available && p.PipesAtFull < p.PipesProbed
}

// String is one startup log line. It is deliberately verbose about the limits
// on a degraded result: the whole point is that the operator can act on it
// before a run rather than discover it in the numbers afterwards.
func (p SpliceProbe) String() string {
	if !p.Available {
		return "splice: unavailable (" + p.Reason + ") — bodies will move through userspace copies"
	}
	s := "splice: available pipe=" + kib(p.FirstSize) +
		" (" + strconv.Itoa(p.PipesAtFull) + "/" + strconv.Itoa(p.PipesProbed) + " pipes at " +
		kib(p.WantPipeSize) + ")"
	if p.Degraded() {
		s = "splice: DEGRADED smallest=" + kib(p.SmallestSize) +
			" (" + strconv.Itoa(p.PipesAtFull) + "/" + strconv.Itoa(p.PipesProbed) + " pipes at " +
			kib(p.WantPipeSize) + ") fs.pipe-max-size=" + sysctlStr(p.MaxSize) +
			" fs.pipe-user-pages-soft=" + sysctlStr(p.SoftPages) +
			" — see bench/RUNBOOK.md §5"
	}
	return s
}

func kib(n int) string {
	if n <= 0 {
		return "?"
	}
	return strconv.Itoa(n/1024) + "KiB"
}

func sysctlStr(n int) string {
	if n < 0 {
		return "?"
	}
	return strconv.Itoa(n)
}
