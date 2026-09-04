package upstream

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeNet stands in for the kernel. The pool never reads or writes an fd, so a
// counter is a complete substitute for a real socket and it additionally proves
// the pool touches the descriptor only through the injected operations.
type fakeNet struct {
	mu     sync.Mutex
	next   int
	open   map[int]bool
	closed []int
	dials  int
	err    error // when non-nil, every dial fails with it
}

func newFakeNet() *fakeNet {
	return &fakeNet{next: 100, open: map[int]bool{}}
}

func (f *fakeNet) dial(string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	if f.err != nil {
		return 0, f.err
	}
	fd := f.next
	f.next++
	f.open[fd] = true
	return fd, nil
}

func (f *fakeNet) close(fd int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open[fd] {
		return errors.New("double close or unknown fd")
	}
	delete(f.open, fd)
	f.closed = append(f.closed, fd)
	return nil
}

func (f *fakeNet) counts() (openN, closedN, dials int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.open), len(f.closed), f.dials
}

// clock is a manually advanced clock so idle expiry and backoff are tested
// without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestPool(t *testing.T, cfg Config) (*Pool, *fakeNet, *clock) {
	t.Helper()
	f := newFakeNet()
	c := &clock{t: time.Unix(1<<30, 0)}
	cfg.Dial = f.dial
	cfg.Close = f.close
	cfg.Now = c.now
	return New("origin:8080", cfg), f, c
}

func TestAcquireDialsThenReuses(t *testing.T) {
	p, f, _ := newTestPool(t, Config{})

	fd, reused, err := p.Acquire()
	if err != nil || reused {
		t.Fatalf("first Acquire = (%d, %v, %v), want a fresh conn", fd, reused, err)
	}
	p.Release(fd)

	fd2, reused, err := p.Acquire()
	if err != nil || !reused {
		t.Fatalf("second Acquire = (%d, %v, %v), want reuse", fd2, reused, err)
	}
	if fd2 != fd {
		t.Fatalf("reused fd = %d, want %d", fd2, fd)
	}
	if _, _, dials := f.counts(); dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
	if s := p.Stats(); s.Reuses != 1 || s.Dials != 1 || s.Idle != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestAcquireDropsExpiredIdle(t *testing.T) {
	p, f, c := newTestPool(t, Config{MaxIdleTime: 10 * time.Second})

	fd, _, _ := p.Acquire()
	p.Release(fd)
	c.advance(11 * time.Second)

	fd2, reused, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	if reused {
		t.Fatal("handed back an expired connection; that costs a retry per request")
	}
	if fd2 == fd {
		t.Fatalf("expired fd %d handed out again", fd)
	}
	openN, closedN, _ := f.counts()
	if closedN != 1 || openN != 1 {
		t.Fatalf("open=%d closed=%d, want the expired fd closed and the new one open", openN, closedN)
	}
	if s := p.Stats(); s.StaleEvicted != 1 {
		t.Fatalf("StaleEvicted = %d, want 1", s.StaleEvicted)
	}
}

func TestReapClosesOnlyExpired(t *testing.T) {
	p, f, c := newTestPool(t, Config{MaxIdleTime: 10 * time.Second})

	// Two connections released at different times. Acquire is not called after
	// the clock advances, so nothing else evicts the old one — this isolates
	// Reap, which is what covers a pool that goes quiet.
	old, _, _ := p.Acquire()
	fresh, _, _ := p.Acquire()
	p.Release(old)
	c.advance(11 * time.Second)
	p.Release(fresh)

	if n := p.Reap(); n != 1 {
		t.Fatalf("Reap = %d, want 1", n)
	}
	if s := p.Stats(); s.Idle != 1 || s.Reaped != 1 {
		t.Fatalf("stats = %+v", s)
	}
	f.mu.Lock()
	closed := append([]int(nil), f.closed...)
	f.mu.Unlock()
	if len(closed) != 1 || closed[0] != old {
		t.Fatalf("closed = %v, want [%d]", closed, old)
	}

	if n := p.Reap(); n != 0 {
		t.Fatalf("second Reap = %d, want 0", n)
	}
	if got, reused, err := p.Acquire(); err != nil || !reused || got != fresh {
		t.Fatalf("Acquire = (%d, %v, %v), want the surviving fd %d", got, reused, err, fresh)
	}
}

func TestReleaseBeyondMaxIdleCloses(t *testing.T) {
	p, f, _ := newTestPool(t, Config{MaxIdle: 2})

	var fds []int
	for range 3 {
		fd, _, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		fds = append(fds, fd)
	}
	for _, fd := range fds {
		p.Release(fd)
	}

	if s := p.Stats(); s.Idle != 2 || s.Overflowed != 1 {
		t.Fatalf("stats = %+v, want Idle 2 and one overflow", s)
	}
	if _, closedN, _ := f.counts(); closedN != 1 {
		t.Fatalf("closed = %d, want 1", closedN)
	}
}

func TestDialFailureBacksOffThenRecovers(t *testing.T) {
	p, f, c := newTestPool(t, Config{BackoffMin: time.Second, BackoffMax: 4 * time.Second})
	boom := errors.New("connection refused")
	f.err = boom

	if _, _, err := p.Acquire(); !errors.Is(err, boom) {
		t.Fatalf("Acquire error = %v, want %v", err, boom)
	}

	// Inside the backoff window no dial is attempted at all.
	if _, _, err := p.Acquire(); !errors.Is(err, ErrUpstreamDown) {
		t.Fatalf("Acquire during backoff = %v, want ErrUpstreamDown", err)
	}
	if _, _, dials := f.counts(); dials != 1 {
		t.Fatalf("dials = %d, want 1 — backoff must suppress the dial", dials)
	}
	if !p.Stats().Down {
		t.Fatal("Stats().Down = false during backoff")
	}

	// Backoff is jittered into [min/2, min), so min is always past it.
	c.advance(time.Second)
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()

	if _, _, err := p.Acquire(); err != nil {
		t.Fatalf("Acquire after recovery: %v", err)
	}
	if s := p.Stats(); s.Down || s.DialFailures != 1 {
		t.Fatalf("stats = %+v, want recovered", s)
	}
	if !p.RetryAt().IsZero() {
		t.Fatalf("RetryAt = %v, want zero after success", p.RetryAt())
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	p, _, _ := newTestPool(t, Config{BackoffMin: time.Second, BackoffMax: 4 * time.Second})

	// Nominal delay doubles per failure; the returned value is jittered into
	// [nominal/2, nominal).
	for _, tc := range []struct {
		failures int
		nominal  time.Duration
	}{
		{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {9, 4 * time.Second},
	} {
		d := p.backoff(tc.failures)
		if d < tc.nominal/2 || d >= tc.nominal {
			t.Errorf("backoff(%d) = %v, want [%v, %v)", tc.failures, d, tc.nominal/2, tc.nominal)
		}
	}
}

func TestPooledConnsStillServedWhileDown(t *testing.T) {
	// A dial failure must not strand healthy pooled connections: refusing them
	// would turn a transient connect failure into a full outage.
	p, f, _ := newTestPool(t, Config{})

	fd, _, _ := p.Acquire()
	p.Release(fd)
	f.mu.Lock()
	f.err = errors.New("refused")
	f.mu.Unlock()

	// Force the pool into backoff using a second, failing Acquire.
	fd2, _, _ := p.Acquire()
	if fd2 != fd {
		t.Fatalf("expected the pooled fd first, got %d", fd2)
	}
	if _, _, err := p.Acquire(); err == nil {
		t.Fatal("expected dial failure")
	}
	p.Release(fd2)

	got, reused, err := p.Acquire()
	if err != nil || !reused || got != fd {
		t.Fatalf("Acquire while down = (%d, %v, %v), want the pooled fd", got, reused, err)
	}
}

func TestCloseClosesIdleAndRejects(t *testing.T) {
	p, f, _ := newTestPool(t, Config{})

	fd, _, _ := p.Acquire()
	p.Release(fd)
	p.Close()

	if openN, closedN, _ := f.counts(); openN != 0 || closedN != 1 {
		t.Fatalf("open=%d closed=%d after Close", openN, closedN)
	}
	if _, _, err := p.Acquire(); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Acquire after Close = %v, want ErrPoolClosed", err)
	}
	p.Close() // idempotent
}

func TestReleaseAfterCloseClosesFD(t *testing.T) {
	p, f, _ := newTestPool(t, Config{})
	fd, _, _ := p.Acquire()
	p.Close()
	p.Release(fd)
	if openN, _, _ := f.counts(); openN != 0 {
		t.Fatalf("open fds after releasing into a closed pool = %d, want 0", openN)
	}
}

// TestConcurrentAcquireRelease is the -race check: it must not double-close an
// fd or hand the same one to two callers at once.
func TestConcurrentAcquireRelease(t *testing.T) {
	p, f, _ := newTestPool(t, Config{MaxIdle: 8})

	var mu sync.Mutex
	held := map[int]bool{}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				fd, _, err := p.Acquire()
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				mu.Lock()
				if held[fd] {
					t.Errorf("fd %d handed out twice", fd)
				}
				held[fd] = true
				mu.Unlock()

				mu.Lock()
				delete(held, fd)
				mu.Unlock()
				p.Release(fd)
			}
		}()
	}
	wg.Wait()

	p.Close()
	if openN, _, _ := f.counts(); openN != 0 {
		t.Fatalf("leaked %d fds", openN)
	}
}
