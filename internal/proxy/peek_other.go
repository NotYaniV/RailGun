//go:build !linux && !darwin

package proxy

import "net"

// pooledConnIsDirty always reports clean on platforms without a non-blocking
// peek. Options.VerifyPooledConns therefore does nothing there, which is
// acceptable because those platforms are explicit non-goals for this project
// and cannot produce a valid measurement anyway.
func pooledConnIsDirty(c *net.TCPConn) bool { return false }
