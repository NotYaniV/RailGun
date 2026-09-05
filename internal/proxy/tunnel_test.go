package proxy_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// upgradeOrigin answers an Upgrade request with 101 and then speaks a trivial
// byte protocol: it echoes what it receives until told to stop.
//
// onUpgraded runs after the 101 has been written, so a test can drive the
// origin side of an established tunnel.
func newUpgradeOrigin(t *testing.T, onUpgraded func(c net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						c.Close()
						return
					}
					if line == "\r\n" {
						break
					}
				}
				io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\n"+
					"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				onUpgraded(c)
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

const upgradeReq = "GET /ws HTTP/1.1\r\nHost: app.test\r\n" +
	"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// The tunnel is the only place in the design that runs two goroutines per
// connection, and it had no test at all. This is the happy path: a 101, then
// bytes in both directions after the upgrade.
func TestTunnelRelaysBothDirections(t *testing.T) {
	origin := newUpgradeOrigin(t, func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		c.Write([]byte("echo:" + string(buf[:n])))
	})
	r := newRigTo(t, testOpts(), origin.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(c, upgradeReq); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading 101: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 101") {
		t.Fatalf("status line %q, want 101", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Post-upgrade the connection is raw bytes in both directions.
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := br.Read(buf)
	if err != nil {
		t.Fatalf("reading tunnelled bytes: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("tunnel returned %q, want %q", got, "echo:ping")
	}
}

// When one side of an established tunnel goes away, the other direction must be
// released too.
//
// tunnel copies down->up in a goroutine and up->down on the caller's goroutine,
// then waits for the goroutine. Deadlines are cleared on entry, deliberately,
// because a tunnel may idle indefinitely by design. So if the ORIGIN closes
// while the client simply holds its socket open — a half-open client, which is
// ordinary — the up->down copy finishes and the down->up copy blocks on a read
// that will never return. The wait for it never ends.
//
// That leaks the serve goroutine, the pooled connState, the slab slot and the
// upstream connection, permanently, for the life of the process. It also means
// s.conns never reaches zero, so Shutdown blocks until its deadline no matter
// how long it is given.
func TestTunnelReleasesWhenOriginClosesFirst(t *testing.T) {
	origin := newUpgradeOrigin(t, func(c net.Conn) {
		// Say one thing, then hang up entirely.
		io.WriteString(c, "bye")
		c.Close()
	})
	r := newRigTo(t, testOpts(), origin.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(c, upgradeReq); err != nil {
		t.Fatal(err)
	}
	// Drain everything the proxy sends: the 101, then "bye", then EOF.
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("draining the tunnel: %v", err)
	}

	// The client deliberately does NOT close. Its write side stays open, which
	// is exactly what a half-open peer looks like.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.srv.UpstreamConnsInUse() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upstream connection still held %v after the origin closed the tunnel; "+
		"the down->up copy is blocked on a client that will never send, and "+
		"tunnel waits for it forever", 3*time.Second)
}

// TestTunnelLargeConcurrentTransfer pushes enough through an established tunnel
// to overflow the kernel socket buffers in both directions at once.
//
// The half-duplex request path can never deadlock on a full socket buffer,
// because only one side is ever writing. A tunnel can: if both peers write more
// than the kernel will buffer while neither is draining, everything stops. So
// this is not covered by using a larger body size anywhere else in the suite —
// it is a property only the concurrent path has.
//
// The test itself has to read and write concurrently for the same reason. Doing
// them in sequence would deadlock against a perfectly correct proxy, and the
// test would be measuring its own shape.
func TestTunnelLargeConcurrentTransfer(t *testing.T) {
	origin := newUpgradeOrigin(t, func(c net.Conn) {
		// Echo everything straight back until the tunnel closes.
		_, _ = io.Copy(c, c)
		c.Close()
	})
	r := newRigTo(t, testOpts(), origin.Addr().String())

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))

	if _, err := io.WriteString(c, upgradeReq); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line %q, want 101", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	const size = 512 << 10
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}

	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(payload)
		werr <- err
	}()

	got := make([]byte, size)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading %d bytes back through the tunnel: %v", size, err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("writing into the tunnel: %v", err)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("tunnel corrupted the stream at offset %d of %d", i, size)
		}
	}
}

// The tunnel teardown closes the upstream socket directly while the pool still
// holds its token, and roundTrip then discards that token, which closes it
// again. That is safe, but "safe" here rests on two separate invariants and it
// is worth pinning them rather than believing them: a second Close on a
// *net.TCPConn only errors, and slab.remove Swaps the slot to nil so the free
// list is appended to exactly once however many times it is called.
//
// A double free is what would actually hurt — the same slot handed to two
// connections at once, so one of them relays onto the other's socket.
// inUse() is len(slots)-len(free), so a double free drives it negative, which
// is the signature this watches for.
func TestTunnelTeardownDoesNotCorruptTheSlab(t *testing.T) {
	origin := newUpgradeOrigin(t, func(c net.Conn) {
		io.WriteString(c, "x")
		c.Close()
	})
	r := newRigTo(t, testOpts(), origin.Addr().String())

	for i := range 5 {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, upgradeReq); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, c); err != nil {
			t.Fatalf("tunnel %d: draining: %v", i, err)
		}
		c.Close()

		deadline := time.Now().Add(3 * time.Second)
		for {
			n := r.srv.UpstreamConnsInUse()
			if n < 0 {
				t.Fatalf("tunnel %d: slab in-use went negative (%d) — a token was "+
					"freed twice and two connections can now share a slot", i, n)
			}
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("tunnel %d: %d upstream connections still held", i, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The slab must still hand out working slots after all that.
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, upgradeReq); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("upgrade after five tunnels failed: %v", err)
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 101") {
		t.Fatalf("got %q, want a 101", buf)
	}
}
