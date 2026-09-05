//go:build linux || darwin

package proxy

import (
	"net"

	"golang.org/x/sys/unix"
)

// pooledConnIsDirty reports whether a connection taken from the idle pool is
// unsafe to reuse: it has readable bytes waiting, or the peer has closed it.
//
// Both conditions mean the same thing — the proxy and the origin no longer
// agree about where the last message ended. Readable bytes on an idle pooled
// connection can only be data no request asked for, and reusing that connection
// hands those bytes to the NEXT request's response parse.
//
// This costs one non-blocking recvmsg per pooled acquire, which is why it is
// off by default. A syscall per request is exactly the currency this project
// is trying to spend less of, so whether it is worth paying is a measurement
// and not a matter of taste. See Options.VerifyPooledConns for the experiment.
//
// MSG_PEEK leaves anything it finds in the socket buffer, so a false positive
// costs a discarded connection and never a lost byte. MSG_DONTWAIT keeps it off
// the netpoll path entirely: the call returns EAGAIN immediately on a healthy
// idle connection, which is the overwhelmingly common case and the one whose
// cost matters.
func pooledConnIsDirty(c *net.TCPConn) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return true
	}

	var (
		n     int
		serr  error
		buf   [1]byte
		flags = unix.MSG_PEEK | unix.MSG_DONTWAIT
	)
	// Returning true from the callback means "do not wait for readiness".
	// Returning false would park this goroutine in netpoll until the connection
	// became readable, which on a healthy idle connection is never.
	if err := rc.Read(func(fd uintptr) bool {
		n, _, serr = unix.Recvfrom(int(fd), buf[:], flags)
		return true
	}); err != nil {
		return true
	}

	switch {
	case serr == unix.EAGAIN || serr == unix.EWOULDBLOCK:
		// Nothing readable: the connection is idle and clean, which is the
		// expected outcome and the only one that lets it be used.
		return false
	case serr != nil:
		return true
	case n == 0:
		// Orderly shutdown by the peer. Reusing this would cost a write, a
		// failed read and a retry; discarding it now costs one dial.
		return true
	default:
		// Bytes nobody asked for.
		return true
	}
}
