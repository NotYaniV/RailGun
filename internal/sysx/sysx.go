//go:build unix

// Package sysx holds the socket-level setup the netpoll data plane needs:
// SO_REUSEPORT listener groups and the shutdown ordering that keeps queued
// SYNs from being dropped.
//
// It lives here rather than in internal/proxy (spec §7 placed the dialer there)
// so that M3's io_uring equivalent becomes a sibling file next to the netpoll
// one instead of a fork of proxy. The I/O-independence invariant of §7.1 binds
// httpx and upstream only; this package is the opposite — it exists precisely
// to concentrate the I/O-model-specific code in one place.
package sysx

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
	"time"
)

// reusePort sets SO_REUSEPORT before bind. It uses golang.org/x/sys/unix, not
// syscall, because syscall.SO_REUSEPORT is undefined on linux/amd64 — the
// syscall package is frozen and never gained the constant on that arch. Spec
// §5.1 already assumes x/sys/unix here.
//
// SO_REUSEADDR and TCP_NODELAY need no
// help: net sets SO_REUSEADDR on every listener, and newTCPConn calls
// setNoDelay(fd, true) on every accepted and dialled connection
// (net/tcpsock.go:290), so an explicit SetNoDelay call would be a no-op.
func reusePort(_, _ string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return opErr
}

// Listen opens n listeners on addr, all members of one SO_REUSEPORT group, so
// the kernel distributes accepts by 4-tuple hash and no userspace accept lock
// exists (spec §5.1). Callers run one accept goroutine per returned listener.
//
// If addr names port 0, the first listener's assigned port is reused for the
// rest — otherwise each would land on a different ephemeral port and the group
// would not be a group. This matters mostly to tests.
//
// The listen backlog is not settable through net: Go passes somaxconn
// (net.listenerBacklog). Matching NGINX's configured backlog=65535 is therefore
// a host concern, see bench/RUNBOOK.md §1.
//
// On failure every listener already opened is closed before returning.
func Listen(addr string, n int) ([]*net.TCPListener, error) {
	if n <= 0 {
		return nil, errors.New("sysx: Listen needs n > 0")
	}
	lc := net.ListenConfig{Control: reusePort}

	lns := make([]*net.TCPListener, 0, n)
	for i := range n {
		l, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			closeAll(lns)
			return nil, err
		}
		tl, ok := l.(*net.TCPListener)
		if !ok {
			l.Close()
			closeAll(lns)
			return nil, errors.New("sysx: listener is not a *net.TCPListener")
		}
		lns = append(lns, tl)

		if i == 0 {
			if _, port, err := net.SplitHostPort(addr); err == nil && port == "0" {
				addr = tl.Addr().String()
			}
		}
	}
	return lns, nil
}

// CloseAfterDrain accepts the connections already queued on lns, handing each
// to accept, and then closes every listener. It returns the number of
// connections drained.
//
// Why this exists: closing a SO_REUSEPORT listener discards the connections
// sitting in its accept queue — clients that completed a handshake get a reset
// for a request the proxy never saw. Draining the queue first is what makes
// shutdown graceful.
//
// A correction to spec §5.7, which says shutdown "removes listeners from the
// reuseport group" and waits so in-flight SYNs land on the remaining ones:
// Linux has no operation that removes a socket from a reuseport group. The
// nearest equivalent is attaching an SO_ATTACH_REUSEPORT_CBPF program that
// stops selecting the draining socket, and its indices shift as group members
// close, which makes it fragile for exactly the case it would be used in.
// There is also nothing to hand off to here: Railgun is a single process, so at
// shutdown the whole group is going away. Draining the queues is therefore the
// whole of the mitigation, and budget is the only knob — hence budget rather
// than a "drain interval".
//
// Passing a nil accept closes drained connections immediately, which is the
// right choice only when the caller is already past its request deadline.
func CloseAfterDrain(lns []*net.TCPListener, budget time.Duration, accept func(*net.TCPConn)) (int, error) {
	deadline := time.Now().Add(budget)

	drained := 0
	for time.Now().Before(deadline) {
		pass := 0
		for _, l := range lns {
			// A queued connection is returned immediately; an empty queue costs
			// this deadline. Short, because it is paid per listener per pass.
			_ = l.SetDeadline(time.Now().Add(2 * time.Millisecond))
			for {
				c, err := l.Accept()
				if err != nil {
					break
				}
				pass++
				tc, _ := c.(*net.TCPConn)
				if accept != nil && tc != nil {
					accept(tc)
				} else {
					c.Close()
				}
			}
		}
		drained += pass
		if pass == 0 {
			// A full sweep found nothing queued. Anything arriving after this
			// point is a SYN we were never going to serve.
			break
		}
	}

	return drained, closeAll(lns)
}

func closeAll(lns []*net.TCPListener) error {
	var errs []error
	for _, l := range lns {
		// Clear the drain deadline so Close is not confused with a timeout by
		// an accept loop still parked on this listener.
		_ = l.SetDeadline(time.Time{})
		if err := l.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
