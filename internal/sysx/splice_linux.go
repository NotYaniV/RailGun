//go:build linux

package sysx

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ProbeSplice reports whether splice(2) works here and what pipe buffer size
// the kernel grants, by making exactly the calls Go's internal/poll makes and
// then reading back what it got.
//
// pipes is how many pipes to hold open simultaneously. This matters: the limit
// that actually bites is fs.pipe-user-pages-soft, a cap on the total pipe pages
// held by the user, so a probe that allocates one pipe and closes it can never
// see it. Pass roughly the number of concurrent large-body relays expected —
// at 1 MiB per in-flight splice, the 64 MiB default soft limit is reached at
// about 64 of them.
//
// The result is a startup-time reading. Under load, other pipes held by the
// same user count against the same limit, so a clean probe is necessary and not
// sufficient — the W4 splice ratio guard remains the check that a real run
// passes or fails on.
func ProbeSplice(pipes int) SpliceProbe {
	if pipes < 1 {
		pipes = 1
	}
	p := SpliceProbe{
		WantPipeSize: SpliceWantPipeSize,
		PipesProbed:  pipes,
		MaxSize:      readSysctlInt("/proc/sys/fs/pipe-max-size"),
		SoftPages:    readSysctlInt("/proc/sys/fs/pipe-user-pages-soft"),
	}

	// Every pipe stays open until the probe ends. Allocating and closing one at
	// a time would never accumulate pages and would report success on a host
	// that degrades at the tenth concurrent relay.
	type pair struct{ r, w int }
	held := make([]pair, 0, pipes)
	defer func() {
		for _, pp := range held {
			unix.Close(pp.r)
			unix.Close(pp.w)
		}
	}()

	for i := range pipes {
		var fds [2]int
		if err := unix.Pipe2(fds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
			p.Reason = "pipe2: " + err.Error()
			return p
		}
		held = append(held, pair{fds[0], fds[1]})

		// The same request Go makes, and like Go we tolerate its failure. The
		// difference is that we then look at what we were given.
		_, _ = unix.FcntlInt(uintptr(fds[0]), unix.F_SETPIPE_SZ, SpliceWantPipeSize)
		sz, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GETPIPE_SZ, 0)
		if err != nil {
			p.Reason = "fcntl(F_GETPIPE_SZ): " + err.Error()
			return p
		}
		if i == 0 {
			p.FirstSize = sz
			p.SmallestSize = sz
		}
		if sz < p.SmallestSize {
			p.SmallestSize = sz
		}
		if sz >= SpliceWantPipeSize {
			p.PipesAtFull++
		}
	}

	// Functional check. A socketpair stands in for a TCP connection: what is
	// being established is that the syscall is permitted and moves bytes
	// socket-to-pipe, which is the half of the relay that a seccomp policy or a
	// sandbox would deny.
	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		p.Reason = "socketpair: " + err.Error()
		return p
	}
	defer unix.Close(sp[0])
	defer unix.Close(sp[1])

	probe := []byte("railgun")
	if _, err := unix.Write(sp[0], probe); err != nil {
		p.Reason = "write: " + err.Error()
		return p
	}
	n, err := unix.Splice(sp[1], nil, held[0].w, nil, len(probe), unix.SPLICE_F_NONBLOCK)
	if err != nil {
		p.Reason = "splice: " + err.Error()
		return p
	}
	if int(n) != len(probe) {
		p.Reason = "splice moved " + strconv.FormatInt(n, 10) + " of " + strconv.Itoa(len(probe)) + " bytes"
		return p
	}

	p.Available = true
	if p.Degraded() {
		p.Reason = "kernel granted " + kib(p.SmallestSize) + " pipes, not " + kib(SpliceWantPipeSize)
	}
	return p
}

// readSysctlInt returns -1 rather than an error: an unreadable limit is
// diagnostic context, never a reason to fail a probe.
func readSysctlInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}
