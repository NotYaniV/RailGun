// Package upstream maintains, per upstream address, a pool of idle connections
// and the passive health state that decides whether a new connection may be
// dialled.
//
// The pool performs no I/O. It stores connections as opaque integer tokens and
// receives its dial and close operations from the caller, so the same pool
// serves the netpoll data plane today and an io_uring one later without
// changing (spec §7.1, §7.2). It must therefore not import net, bufio, os or
// golang.org/x/sys/unix; an import-guard test enforces that.
//
// # The token invariant
//
// The int this package stores, ages, reaps and hands back is defined entirely
// by the caller. The pool performs no operation on it beyond passing it to the
// injected Dial and Close, and nothing here may assume it is a file descriptor.
//
// This is load-bearing, not pedantry. Under Approach A the token is an index
// into the proxy's slab of *net.TCPConn, because splice(2) only happens inside
// (*net.TCPConn).ReadFrom — converting a real fd back to a TCPConn would cost a
// dup(2) and a fresh epoll registration on every request, adding syscalls to
// the one path whose purpose is removing them. Under Approach B the same int
// becomes an index into an io_uring registered-file table. Both work only
// because the pool never interprets the value.
//
// That guard is transitive, and os is reachable from fmt, so this package
// formats its errors by hand rather than with fmt.Errorf. Any package under the
// same invariant has to do likewise.
//
// This is variant A of experiment E1 (spec §5.4): one sync.Mutex per upstream
// over a slice of idle fds. Sharding is deliberately absent. If a -mutex or
// -block profile shows no contention here, that null result is the finding and
// variants B and C are never built.
package upstream

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// Dial opens a new connection to addr and returns the caller-defined token
// identifying it. It is injected because dialling requires net or unix, which
// this package may not import.
type Dial func(addr string) (token int, err error)

// Close releases a token's connection and any slot the caller holds for it.
// Injected for the same reason as Dial, and because a completion-based data
// plane releases a connection by submitting to a ring rather than by calling
// close(2).
type Close func(token int) error

var (
	// ErrUpstreamDown means the upstream is in dial backoff after a connect
	// failure. No dial was attempted. Callers holding several members of an
	// upstream group should try the next member.
	ErrUpstreamDown = errors.New("upstream: down, in dial backoff")

	// ErrPoolClosed means Close has been called on the pool.
	ErrPoolClosed = errors.New("upstream: pool closed")
)

// DialError reports a failed dial to an upstream. It exists instead of a
// fmt.Errorf wrap because fmt transitively imports os, which the §7.1 import
// guard forbids — see the note in the package comment.
type DialError struct {
	Addr string
	Err  error
}

func (e *DialError) Error() string { return "upstream: dial " + e.Addr + ": " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// Config configures a Pool. The zero value of every duration and count field
// selects the default noted on it.
type Config struct {
	Dial  Dial  // required
	Close Close // required

	// MaxIdle caps pooled idle connections. Releases beyond it are closed
	// rather than pooled. Default 32.
	MaxIdle int

	// MaxIdleTime is how long a connection may sit idle before it is assumed to
	// have been closed by the upstream. Handing out a conn the upstream has
	// already FIN'd costs a failed write and a retry on every request, so
	// expired entries are dropped on Acquire as well as by Reap. Default 30s.
	//
	// Set this below the upstream's own keepalive timeout. NGINX defaults
	// keepalive_timeout to 75s upstream-side; a proxy that pools for longer
	// than the origin holds is racing the origin's close on every request.
	MaxIdleTime time.Duration

	// BackoffMin and BackoffMax bound the exponential backoff applied after a
	// dial failure. Defaults 50ms and 5s.
	BackoffMin time.Duration
	BackoffMax time.Duration

	// Now overrides the clock. Tests set it; production leaves it nil.
	Now func() time.Time
}

// Stats is a snapshot of pool counters. It exists to answer the E1 gating
// question (spec §5.4) with numbers rather than opinion: Dials rising while
// Idle is non-zero is the pool fragmentation that gates variant C.
type Stats struct {
	Idle         int    // connections currently pooled
	Reuses       uint64 // Acquires served from the pool
	Dials        uint64 // Acquires that opened a new connection
	DialFailures uint64
	StaleEvicted uint64 // idle conns dropped by Acquire for exceeding MaxIdleTime
	Reaped       uint64 // idle conns dropped by Reap for the same reason
	Overflowed   uint64 // Releases closed because the pool was full
	Down         bool   // currently in dial backoff
}

type entry struct {
	fd        int
	idleSince time.Time
}

// Pool is one upstream address's idle connections plus its health state. All
// methods are safe for concurrent use.
type Pool struct {
	addr        string
	dial        Dial
	closeFD     Close
	maxIdle     int
	maxIdleTime time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration
	now         func() time.Time

	mu sync.Mutex
	// idle is ordered oldest-first: Release appends, so idleSince ascends with
	// index. Acquire pops the tail (LIFO) to keep hot connections hot and let
	// cold ones age out at the head, where Reap sweeps them.
	idle     []entry
	failures int
	retryAt  time.Time
	closed   bool
	stats    Stats
}

// New returns a pool for addr. It panics if Dial or Close is nil, since a pool
// that cannot open or close a connection is never a recoverable runtime state.
func New(addr string, cfg Config) *Pool {
	if cfg.Dial == nil || cfg.Close == nil {
		panic("upstream: Config.Dial and Config.Close are required")
	}
	p := &Pool{
		addr:        addr,
		dial:        cfg.Dial,
		closeFD:     cfg.Close,
		maxIdle:     cfg.MaxIdle,
		maxIdleTime: cfg.MaxIdleTime,
		backoffMin:  cfg.BackoffMin,
		backoffMax:  cfg.BackoffMax,
		now:         cfg.Now,
	}
	if p.maxIdle <= 0 {
		p.maxIdle = 32
	}
	if p.maxIdleTime <= 0 {
		p.maxIdleTime = 30 * time.Second
	}
	if p.backoffMin <= 0 {
		p.backoffMin = 50 * time.Millisecond
	}
	if p.backoffMax < p.backoffMin {
		p.backoffMax = 5 * time.Second
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

// Addr reports the upstream address this pool serves.
func (p *Pool) Addr() string { return p.addr }

// Acquire returns a connection to the upstream. reused reports whether it came
// from the pool, which is the datum the caller needs to decide retries: a
// pooled connection can be closed by the upstream between Release and the next
// write, so a failure on a reused connection is a keepalive race and an
// idempotent request may be retried once on a fresh one. A failure on a freshly
// dialled connection is a real error and must surface.
//
// The dial happens outside the lock, so a slow connect never blocks other
// workers acquiring pooled connections.
func (p *Pool) Acquire() (fd int, reused bool, err error) {
	now := p.now()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, false, ErrPoolClosed
	}

	// Pop the freshest idle fd. Because idleSince ascends with index, the first
	// entry found to be expired means every entry below it is expired too.
	var stale []int
	for i := len(p.idle) - 1; i >= 0; i-- {
		e := p.idle[i]
		p.idle = p.idle[:i]
		if now.Sub(e.idleSince) < p.maxIdleTime {
			p.stats.Reuses++
			p.mu.Unlock()
			p.closeAll(stale)
			return e.fd, true, nil
		}
		p.stats.StaleEvicted++
		stale = append(stale, e.fd)
	}

	if now.Before(p.retryAt) {
		p.mu.Unlock()
		p.closeAll(stale)
		return 0, false, ErrUpstreamDown
	}
	p.mu.Unlock()
	p.closeAll(stale)

	// ponytail: no cap on concurrent dials. A cold pool under a burst opens as
	// many connections as there are in-flight requests. Add a semaphore here if
	// a run shows connect storms against the origin.
	fd, derr := p.dial(p.addr)

	p.mu.Lock()
	if derr != nil {
		p.failures++
		p.stats.DialFailures++
		p.retryAt = p.now().Add(p.backoff(p.failures))
		p.mu.Unlock()
		return 0, false, &DialError{Addr: p.addr, Err: derr}
	}
	p.failures = 0
	p.retryAt = time.Time{}
	p.stats.Dials++
	closed := p.closed
	p.mu.Unlock()

	if closed {
		p.closeFD(fd)
		return 0, false, ErrPoolClosed
	}
	return fd, false, nil
}

// Release returns a healthy connection to the pool. The caller must not release
// a connection it has any reason to believe is broken, or one whose response
// framing it did not fully consume — use Discard for those.
func (p *Pool) Release(token int) {
	// Read the clock before taking the lock. time.Now costs on the order of
	// 25ns, which is the same order as the entire critical section it would
	// otherwise sit inside — every request passes through here, so leaving it
	// under the mutex would roughly double the contended hold time and E1 would
	// then be measuring the clock rather than the pool.
	now := p.now()

	p.mu.Lock()
	if p.closed || len(p.idle) >= p.maxIdle {
		if !p.closed {
			p.stats.Overflowed++
		}
		p.mu.Unlock()
		p.closeFD(token)
		return
	}
	p.idle = append(p.idle, entry{fd: token, idleSince: now})
	p.mu.Unlock()
}

// Discard closes a connection instead of pooling it.
//
// It deliberately does not mark the upstream down. A reused connection dying is
// an ordinary keepalive race, not evidence about the upstream's health; only a
// failed dial is (spec §5.4, passive health).
func (p *Pool) Discard(fd int) {
	p.closeFD(fd)
}

// Reap closes idle connections that have exceeded MaxIdleTime and returns how
// many it closed. Acquire already drops expired fds, so Reap exists for the
// pool that goes quiet: without it, an idle upstream holds its fds until
// traffic returns. Call it from a coarse timer, once a second is plenty.
func (p *Pool) Reap() int {
	cutoff := p.now().Add(-p.maxIdleTime)

	p.mu.Lock()
	// Oldest-first ordering means the expired entries are a prefix.
	n := 0
	for n < len(p.idle) && p.idle[n].idleSince.Before(cutoff) {
		n++
	}
	if n == 0 {
		p.mu.Unlock()
		return 0
	}
	expired := make([]int, n)
	for i := range expired {
		expired[i] = p.idle[i].fd
	}
	p.idle = append(p.idle[:0], p.idle[n:]...)
	p.stats.Reaped += uint64(n)
	p.mu.Unlock()

	p.closeAll(expired)
	return n
}

// Close closes every pooled connection and makes the pool permanently
// unusable. Connections currently checked out are not affected; the caller
// still owns those and must Discard them.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	fds := make([]int, len(p.idle))
	for i, e := range p.idle {
		fds[i] = e.fd
	}
	p.idle = nil
	p.mu.Unlock()

	p.closeAll(fds)
}

// Stats returns a snapshot of the pool's counters.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Idle = len(p.idle)
	s.Down = p.now().Before(p.retryAt)
	return s
}

// RetryAt reports when dialling will be attempted again. It is the zero time
// when the upstream is healthy.
func (p *Pool) RetryAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retryAt
}

// backoff returns the delay before the next dial attempt after n consecutive
// failures. Jitter is not decoration: every worker process holds its own pool
// for the same upstream, and without it they all retry on the same instant and
// re-converge into a synchronised connect storm.
func (p *Pool) backoff(n int) time.Duration {
	d := p.backoffMin
	for i := 1; i < n && d < p.backoffMax; i++ {
		d *= 2
	}
	if d > p.backoffMax {
		d = p.backoffMax
	}
	// Uniform in [d/2, d).
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	return time.Duration(half + rand.Int64N(half))
}

func (p *Pool) closeAll(tokens []int) {
	for _, tok := range tokens {
		p.closeFD(tok)
	}
}
