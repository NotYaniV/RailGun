package proxy

import (
	"net"
	"sync"
	"sync/atomic"
)

// connSlab maps the opaque integer tokens held by internal/upstream to the
// *net.TCPConn they stand for.
//
// This indirection is the load-bearing detail of the Approach A upstream path,
// so it is worth stating plainly why it exists.
//
// Design spec section 7.1 requires internal/upstream to store integers and
// receive an injected dialer, so that the io_uring data plane in M3 can reuse
// the pool verbatim with the integer meaning a registered-file index. Under
// Approach A the integer cannot be a real file descriptor: splice(2) only
// happens inside (*net.TCPConn).ReadFrom, so a raw fd would have to be
// converted back with net.FileConn on every acquire, costing a dup(2) and a
// fresh epoll registration per request. Adding two syscalls per request to the
// path whose entire thesis is removing syscalls would forfeit the result
// before M3 begins.
//
// So the token is an index into this slab and nothing else. The pool never
// performs any operation on it; it only hands it back to the injected Dial and
// Close. Under the ring, the same integer becomes a registered-file index and
// this slab disappears without the pool noticing.
//
// Lookup is lock-free by construction. The slot array is allocated once at
// startup and never resized, so a reader loading a slot races with nothing.
// The free list is guarded by a mutex, but it is touched only on dial and on
// close — never on the per-request acquire/release path, which moves tokens
// through the pool's own bookkeeping.
type connSlab struct {
	slots []atomic.Pointer[net.TCPConn]

	mu   sync.Mutex
	free []int32
}

func newConnSlab(n int) *connSlab {
	s := &connSlab{
		slots: make([]atomic.Pointer[net.TCPConn], n),
		free:  make([]int32, n),
	}
	// Hand out low indices first so the working set stays dense and the hot
	// slots share cache lines instead of being scattered across the array.
	for i := range s.free {
		s.free[i] = int32(n - 1 - i)
	}
	return s
}

// errSlabFull is returned when every slot is in use. It surfaces as a dial
// failure, which is the honest description: the proxy has run out of room to
// track upstream connections and must not open another.
var errSlabFull = &slabError{"proxy: upstream connection slab exhausted"}

type slabError struct{ msg string }

func (e *slabError) Error() string { return e.msg }

// put stores c and returns its token.
func (s *connSlab) put(c *net.TCPConn) (int, error) {
	s.mu.Lock()
	if len(s.free) == 0 {
		s.mu.Unlock()
		return 0, errSlabFull
	}
	i := s.free[len(s.free)-1]
	s.free = s.free[:len(s.free)-1]
	s.mu.Unlock()

	s.slots[i].Store(c)
	return int(i), nil
}

// get returns the connection for a token, or nil if the token is stale.
func (s *connSlab) get(tok int) *net.TCPConn {
	if tok < 0 || tok >= len(s.slots) {
		return nil
	}
	return s.slots[tok].Load()
}

// remove clears a token and returns the connection it held, releasing the slot
// for reuse. It returns nil if the slot was already empty, so a double close
// is a no-op rather than a double free of the index.
func (s *connSlab) remove(tok int) *net.TCPConn {
	if tok < 0 || tok >= len(s.slots) {
		return nil
	}
	c := s.slots[tok].Swap(nil)
	if c == nil {
		return nil
	}
	s.mu.Lock()
	s.free = append(s.free, int32(tok))
	s.mu.Unlock()
	return c
}

// inUse reports how many slots are occupied. Test and diagnostic use only.
func (s *connSlab) inUse() int {
	s.mu.Lock()
	n := len(s.slots) - len(s.free)
	s.mu.Unlock()
	return n
}
