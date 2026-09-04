// Package proxy implements the Approach A data plane: one goroutine per
// downstream connection, driven by the Go runtime's netpoll.
//
// The architectural bet is stated as hypothesis H1 in design spec 5.1.1 and is
// unproven: that under CPU-skewed load, Go's work-stealing scheduler yields a
// lower p99.9 than NGINX's fixed accept-time worker affinity, because an idle
// P can steal a connection's goroutine regardless of which listener accepted
// it, whereas NGINX cannot migrate a connection between workers after accept.
// This package is the instrument that measures that, not a proof of it.
package proxy

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/httpx"
	"github.com/NotYaniV/RailGun/internal/metrics"
	"github.com/NotYaniV/RailGun/internal/upstream"
)

// Options configures a Server. The zero value is not usable; use DefaultOptions.
type Options struct {
	// ReadBufferSize is the per-connection downstream read buffer, and thus
	// the hard ceiling on a request head. A request whose head exceeds it is
	// rejected with 431 rather than growing the buffer, per spec 5.3: this is
	// a trust-boundary limit, not a tuning knob.
	ReadBufferSize int
	// UpstreamBufferSize is the equivalent ceiling on a response head.
	UpstreamBufferSize int
	// CopyBufferSize is the userspace relay buffer used for chunked bodies and
	// for the -force-copy control arm of experiment E2.
	CopyBufferSize int

	// SpliceThreshold is the body size at or above which a Content-Length body
	// is moved with splice(2) instead of a userspace copy.
	//
	// Spec 5.5 predicts a crossover: below some size the copy wins, because
	// pushing bytes through the intermediate pipe costs two ring round-trips
	// that a single read/write pair into an L1-resident buffer does not.
	// Experiment E2 measures where that crossover actually is. Until it does,
	// this default is a guess and is labelled as one.
	SpliceThreshold int64

	// ForceCopy disables splice entirely. This is the control arm of E2, not
	// a fallback: the point is a paired A/B in the same session on the same
	// rig, so that "splice happened" can be separated from "splice won".
	ForceCopy bool

	// MaxUpstreamConns bounds the connection slab and therefore the number of
	// simultaneous upstream connections across all pools.
	MaxUpstreamConns int

	// IdleTimeout bounds how long a downstream keepalive connection may sit
	// between requests before the proxy closes it.
	IdleTimeout time.Duration
	// ReadTimeout bounds reading one request head.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing one response.
	WriteTimeout time.Duration
	// UpstreamTimeout bounds a single upstream operation.
	UpstreamTimeout time.Duration

	// ClockInterval is the update period of the cached coarse clock that
	// feeds timeout arithmetic. See coarseClock for why this exists and what
	// precision it costs.
	ClockInterval time.Duration

	// PoolConfig supplies the idle/backoff policy for every upstream pool.
	// Dial and Close are filled in by the Server; anything set here is
	// overwritten.
	PoolConfig upstream.Config

	// Dialer opens upstream connections. Injected so tests need no network.
	Dialer func(addr string) (*net.TCPConn, error)
}

// DefaultOptions returns the tuned defaults.
//
// The buffer sizes spend memory deliberately. Spec section 2 declares resident
// memory the sacrifice, bounded at 4x NGINX's RSS and enforced by GOMEMLIMIT
// rather than discovered afterwards. That bound is not a formality: a working
// set larger than L3 pays for itself in cache misses at exactly the request
// rate the memory was spent to buy, which is why LLC-miss rate is recorded on
// every run.
func DefaultOptions() Options {
	return Options{
		ReadBufferSize:     16 << 10,
		UpstreamBufferSize: 16 << 10,
		CopyBufferSize:     64 << 10,
		// Guess, pending E2. Sized so a 1 KiB W1 response takes the copy path
		// and a 1 MiB W4 response takes splice, which is the hypothesis the
		// experiment exists to test rather than assume.
		SpliceThreshold:  32 << 10,
		MaxUpstreamConns: 1 << 16,
		IdleTimeout:      75 * time.Second,
		ReadTimeout:      15 * time.Second,
		WriteTimeout:     15 * time.Second,
		UpstreamTimeout:  30 * time.Second,
		ClockInterval:    time.Millisecond,
		PoolConfig: upstream.Config{
			MaxIdle: 64,
			// Must stay below the origin's keepalive_timeout, or the proxy
			// races the origin's close on every reuse.
			MaxIdleTime: 30 * time.Second,
			BackoffMin:  50 * time.Millisecond,
			BackoffMax:  5 * time.Second,
		},
	}
}

// Server is the proxy data plane.
type Server struct {
	opts  Options
	cfg   *config.Store
	slab  *connSlab
	clock *coarseClock
	state sync.Pool

	mu    sync.Mutex
	pools map[string]*upstream.Pool

	// closing stops new accepts and new keepalive rounds. Either StopAccepting
	// or Shutdown may set it, so it cannot also serve as Shutdown's
	// once-only guard — shutdownOnce does that. Conflating the two made
	// Shutdown a no-op whenever the documented drain order was followed.
	closing      atomic.Bool
	shutdownOnce atomic.Bool
	conns        sync.WaitGroup
	done         chan struct{}
}

// New builds a Server over an existing config store.
func New(cfg *config.Store, opts Options) *Server {
	if opts.Dialer == nil {
		opts.Dialer = defaultDialer
	}
	if opts.ClockInterval <= 0 {
		opts.ClockInterval = time.Millisecond
	}
	s := &Server{
		opts:  opts,
		cfg:   cfg,
		slab:  newConnSlab(opts.MaxUpstreamConns),
		clock: newCoarseClock(opts.ClockInterval),
		pools: make(map[string]*upstream.Pool),
		done:  make(chan struct{}),
	}
	s.state.New = func() any { return newConnState(&opts) }
	return s
}

func defaultDialer(addr string) (*net.TCPConn, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		c.Close()
		return nil, errors.New("proxy: dialer returned a non-TCP connection")
	}
	// Nagle would batch a small response head with whatever follows and add up
	// to a round trip of latency to exactly the p99.9 figure being measured.
	_ = tc.SetNoDelay(true)
	return tc, nil
}

// pool returns the pool for an upstream address, creating it on first use.
//
// One pool per address, and per spec 5.4 variant A only: a single mutex over a
// slice of idle connections. Sharding is deliberately not built. It is gated
// on experiment E1 showing real contention in a -mutex or -block profile,
// because building it first would reproduce NGINX's known pool fragmentation
// before proving the contention it is meant to fix exists.
func (s *Server) pool(addr string) *upstream.Pool {
	s.mu.Lock()
	p, ok := s.pools[addr]
	if !ok {
		cfg := s.opts.PoolConfig
		// The pool reads the clock twice per request: once in Acquire to age
		// out expired idle entries, once in Release to stamp the returned one.
		// Handing it the cached clock is where most of that cost goes away.
		cfg.Now = s.clock.Now
		cfg.Dial = func(a string) (int, error) {
			c, err := s.opts.Dialer(a)
			if err != nil {
				return 0, err
			}
			tok, err := s.slab.put(c)
			if err != nil {
				c.Close()
				return 0, err
			}
			metrics.IncUpstreamDial()
			return tok, nil
		}
		cfg.Close = func(tok int) error {
			if c := s.slab.remove(tok); c != nil {
				return c.Close()
			}
			return nil
		}
		p = upstream.New(addr, cfg)
		s.pools[addr] = p
	}
	s.mu.Unlock()
	return p
}

// Serve accepts on every listener until Shutdown is called.
//
// One accept goroutine per listener. The listeners are expected to be a
// SO_REUSEPORT group of GOMAXPROCS members, so the kernel's 4-tuple hash does
// the distribution and no userspace accept lock exists at all — which is how
// this design sidesteps NGINX's accept_mutex question rather than answering
// it. Two caveats the spec requires be documented rather than hidden: the hash
// distributes unevenly at low connection counts, which is precisely the
// premise workload W2 depends on; and closing a reuseport listener can drop
// already-queued SYNs, which is why Shutdown drains before closing.
func (s *Server) Serve(lns []*net.TCPListener) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(lns))

	for _, ln := range lns {
		wg.Add(1)
		go func(ln *net.TCPListener) {
			defer wg.Done()
			for {
				c, err := ln.AcceptTCP()
				if err != nil {
					if s.closing.Load() {
						return
					}
					errs <- err
					return
				}
				s.ServeConn(c)
			}
		}(ln)
	}

	wg.Wait()
	close(errs)
	return <-errs
}

// StopAccepting marks the server as shutting down without waiting for
// anything. It is idempotent.
//
// This must be called BEFORE draining the listener accept queues, and the
// ordering is not cosmetic. sysx.CloseAfterDrain sets a short deadline on each
// listener to poll its queue, which makes any accept goroutine already parked
// in AcceptTCP return a timeout error. Without this flag already set, Serve
// reads that timeout as a real accept failure, reports it, and tears down the
// accept loop mid-drain. With it set, the accept goroutines exit quietly and
// the drain proceeds.
//
// The two can still race for the same queued connection. That is harmless:
// whichever wins hands it to ServeConn and it is served either way. Only the
// count CloseAfterDrain reports is affected, which is a diagnostic, not a
// guarantee.
func (s *Server) StopAccepting() { s.closing.Store(true) }

// ServeConn takes ownership of an already-accepted connection and drives it.
//
// It exists for the shutdown path. Closing a SO_REUSEPORT listener discards
// connections the kernel has already queued on it, and those clients completed
// a handshake — they get a reset for a request the proxy never read. Draining
// means accepting the queue and serving it, which needs a way to hand an
// accepted connection back in.
func (s *Server) ServeConn(c *net.TCPConn) {
	// TCP_NODELAY is already set: net.newTCPConn enables it on every accepted
	// and dialled connection, so setting it here again would be a redundant
	// setsockopt on the accept path.
	s.conns.Add(1)
	go s.serve(c)
}

// Shutdown stops accepting and drains in-flight connections.
//
// Order matters and follows spec 5.7. Listeners are closed first so no new
// connection is accepted, but a reuseport listener close can discard SYNs the
// kernel has already queued, so the caller is expected to have removed this
// instance from the group and waited a drain interval before getting here.
// Shutdown is safe to call after StopAccepting, and the drain order documented
// on StopAccepting requires exactly that. It therefore guards itself with its
// own flag rather than with closing: closing is already true by the time the
// caller gets here, so a CompareAndSwap on it would return early and skip the
// drain, the pool close and the done signal — turning graceful shutdown into
// an immediate exit while still logging that it drained.
func (s *Server) Shutdown(ctx <-chan struct{}) {
	s.closing.Store(true)
	if !s.shutdownOnce.CompareAndSwap(false, true) {
		return
	}
	drained := make(chan struct{})
	go func() {
		s.conns.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx:
	}

	s.mu.Lock()
	for _, p := range s.pools {
		p.Close()
	}
	s.mu.Unlock()
	s.clock.Close()
	close(s.done)
}

// ReapIdle sweeps every pool for expired idle connections. The caller runs it
// on a ticker; it is not on any request path.
func (s *Server) ReapIdle() int {
	s.mu.Lock()
	pools := make([]*upstream.Pool, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p)
	}
	s.mu.Unlock()

	n := 0
	for _, p := range pools {
		n += p.Reap()
	}
	return n
}

// PoolStats returns per-upstream pool statistics, keyed by address.
//
// Dials rising while Idle is non-zero is the fragmentation signal that gates
// pool variant C in spec 5.4, so it is measurable from the first run rather
// than after someone thinks to add a counter.
func (s *Server) PoolStats() map[string]upstream.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]upstream.Stats, len(s.pools))
	for addr, p := range s.pools {
		out[addr] = p.Stats()
	}
	return out
}

// UpstreamConnsInUse reports occupied slab slots.
func (s *Server) UpstreamConnsInUse() int { return s.slab.inUse() }

// serve drives one downstream connection for its whole lifetime.
//
// Per spec 5.2 this is ONE goroutine, not two. HTTP/1.1 without upgrades is
// half-duplex — the request completes before the response begins — so a single
// goroutine drives the whole exchange sequentially. A second goroutine is
// spawned only for genuinely bidirectional traffic: CONNECT and Upgrade. That
// halves goroutine count and scheduler churn against the obvious design.
//
// The body of this function reads as ordinary blocking code while the runtime
// does the epoll bookkeeping underneath. Whether that abstraction is free is
// the thing this project is measuring.
func (s *Server) serve(down *net.TCPConn) {
	defer s.conns.Done()
	defer down.Close()

	st := s.state.Get().(*connState)
	defer func() {
		st.reset()
		s.state.Put(st)
	}()

	for i := 0; ; i++ {
		// The shutdown check guards KEEPALIVE continuation, not the first
		// exchange. A connection handed in by the drain path was already
		// accepted and is owed the one request it queued for; refusing it
		// here would reset exactly the clients draining exists to protect.
		// Subsequent keepalive rounds are declined so the connection winds
		// down instead of holding the drain open.
		if i > 0 && s.closing.Load() {
			return
		}
		keepAlive, err := s.exchange(down, st)
		if err != nil || !keepAlive {
			return
		}
	}
}

// errClosed marks a clean end-of-connection rather than a failure.
var errClosed = errors.New("proxy: connection closed")

// idempotent reports whether a method may be safely retried on a fresh
// upstream connection after a failure on a reused one.
//
// The distinction matters because a pooled connection can be closed by the
// origin between the moment it was pooled and the moment it is written to,
// and that race is indistinguishable from a real upstream failure without the
// `reused` flag the pool returns. NGINX retries in this situation by default
// via proxy_next_upstream; not matching that policy would have NGINX report
// zero errors on a workload where Railgun reports some, which is a benchmark
// artifact rather than a difference in the proxies.
func idempotent(method []byte) bool {
	switch string(method) {
	case "GET", "HEAD", "PUT", "DELETE", "OPTIONS", "TRACE":
		return true
	}
	return false
}

// writeError sends a pre-rendered error response. The bytes are constant, so
// the failure path allocates nothing — a failure path that allocates is one
// that degrades under exactly the load that triggers it.
func (s *Server) writeError(down *net.TCPConn, status int) {
	if d := s.opts.WriteTimeout; d > 0 {
		_ = down.SetDeadline(s.clock.Now().Add(d))
	}
	_, _ = down.Write(httpx.ErrorResponse(status))
}
