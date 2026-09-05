//go:build unix

package sysx

import (
	"errors"
	"net"
	"runtime"
	"strconv"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"testing"
	"time"
)

func TestListenSharesOnePort(t *testing.T) {
	lns, err := Listen("127.0.0.1:0", 4)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer closeAll(lns)

	if len(lns) != 4 {
		t.Fatalf("got %d listeners, want 4", len(lns))
	}
	want := lns[0].Addr().String()
	for i, l := range lns {
		if got := l.Addr().String(); got != want {
			t.Fatalf("listener %d bound %s, want %s — the group is not a group", i, got, want)
		}
	}

	// SO_REUSEPORT is actually set, not just assumed because bind succeeded.
	for i, l := range lns {
		rc, err := l.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var v int
		var serr error
		if err := rc.Control(func(fd uintptr) {
			v, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT)
		}); err != nil {
			t.Fatal(err)
		}
		if serr != nil {
			t.Fatal(serr)
		}
		if v == 0 {
			t.Fatalf("listener %d has SO_REUSEPORT unset", i)
		}
	}
}

func TestListenAcceptsAcrossTheGroup(t *testing.T) {
	lns, err := Listen("127.0.0.1:0", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(lns)
	addr := lns[0].Addr().String()

	// One accept goroutine per listener, as the proxy will run them.
	got := make(chan int, 1)
	for i, l := range lns {
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
			select {
			case got <- i:
			default:
			}
		}()
	}

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	c.Close()

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no listener in the reuseport group accepted the connection")
	}
}

func TestListenRejectsBadN(t *testing.T) {
	if _, err := Listen("127.0.0.1:0", 0); err == nil {
		t.Fatal("Listen with n=0 returned no error")
	}
}

func TestCloseAfterDrainServesQueuedConns(t *testing.T) {
	lns, err := Listen("127.0.0.1:0", 2)
	if err != nil {
		t.Fatal(err)
	}
	addr := lns[0].Addr().String()

	// Queue connections without accepting them. These are exactly the ones a
	// plain Close would discard.
	var dialed []net.Conn
	for range 3 {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		dialed = append(dialed, c)
	}
	defer func() {
		for _, c := range dialed {
			c.Close()
		}
	}()

	served := 0
	n, err := CloseAfterDrain(lns, time.Second, func(c *net.TCPConn) {
		served++
		c.Close()
	})
	if err != nil {
		t.Fatalf("CloseAfterDrain: %v", err)
	}
	if n != 3 || served != 3 {
		t.Fatalf("drained %d, served %d, want 3 and 3 — queued conns were dropped", n, served)
	}

	// Listeners are closed: the port no longer accepts.
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("port still accepting after CloseAfterDrain")
	}
}

func TestCloseAfterDrainWithEmptyQueue(t *testing.T) {
	lns, err := Listen("127.0.0.1:0", 4)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing queued: it must return promptly rather than burning the budget.
	start := time.Now()
	n, err := CloseAfterDrain(lns, 5*time.Second, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("drained %d, want 0", n)
	}
	if elapsed > time.Second {
		t.Fatalf("took %v on an empty queue; it must not wait out the budget", elapsed)
	}
}

func TestCloseAfterDrainUnblocksAcceptLoop(t *testing.T) {
	// An accept goroutine parked on Accept must see a permanent error after
	// shutdown, not a deadline timeout it would retry forever.
	lns, err := Listen("127.0.0.1:0", 1)
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() {
		for {
			c, err := lns[0].Accept()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue // a drain deadline, not shutdown
				}
				errc <- err
				return
			}
			c.Close()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	if _, err := CloseAfterDrain(lns, time.Second, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("accept loop exited with %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accept loop never unblocked after shutdown")
	}
}

// TestListenDistributesAcrossTheGroup pins the premise of spec §5.1: the kernel
// spreads accepts across the reuseport group by 4-tuple hash, which is what
// replaces NGINX's accept_mutex question rather than answering it.
//
// If the group silently collapsed to a single listener — a Control function
// that failed quietly, a kernel without SO_REUSEPORT load balancing — every
// benchmark number would still look entirely plausible and the architecture
// being measured would not be the one described.
func TestListenDistributesAcrossTheGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		// SO_REUSEPORT on the BSDs and darwin permits multiple binds but does
		// not load balance across them; FreeBSD needs SO_REUSEPORT_LB for that
		// and darwin has no equivalent. So a development run on a Mac has one
		// effective listener however many were opened, and the distribution
		// this asserts can only be checked on the platform the rig runs.
		t.Skip("SO_REUSEPORT load balancing is Linux-only; darwin delivers to one socket")
	}

	const listeners = 4
	const dials = 200

	lns, err := Listen("127.0.0.1:0", listeners)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(lns)
	addr := lns[0].Addr().String()

	counts := make([]atomic.Int64, listeners)
	for i, l := range lns {
		go func(i int, l *net.TCPListener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				counts[i].Add(1)
				c.Close()
			}
		}(i, l)
	}

	for range dials {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.Close()
	}

	// Accepts are asynchronous; wait for the count to settle rather than
	// sleeping a guessed interval.
	deadline := time.Now().Add(5 * time.Second)
	var total int64
	for time.Now().Before(deadline) {
		total = 0
		for i := range counts {
			total += counts[i].Load()
		}
		if total >= dials {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	used, detail := 0, ""
	for i := range counts {
		n := counts[i].Load()
		if n > 0 {
			used++
		}
		detail += " " + strconv.Itoa(i) + "=" + strconv.FormatInt(n, 10)
	}
	t.Logf("accepted %d of %d dials across %d listeners:%s", total, dials, used, detail)

	if used < 2 {
		t.Fatalf("every one of %d connections landed on a single listener (%s); "+
			"the reuseport group is not distributing and spec §5.1's premise "+
			"does not hold on this host", dials, detail)
	}
}
