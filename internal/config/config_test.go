package config

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func testSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	snap, err := NewSnapshot(1,
		[]Route{
			{Host: "example.com", Upstream: "api"},
			{Host: "other.example.com", Upstream: "api"},
		},
		[]Upstream{
			{Name: "api", Addrs: []string{"127.0.0.1:9000", "127.0.0.1:9001"}},
		},
	)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	return snap
}

func TestRouteMatch(t *testing.T) {
	snap := testSnapshot(t)

	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"example.com", "api", true},
		{"EXAMPLE.COM", "api", true},
		{"example.com:8443", "api", true},
		{"Example.Com:443", "api", true},
		{"other.example.com", "api", true},
		{"[::1]:8080", "", false},
		{"nope.example.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		u, ok := snap.Route([]byte(c.host))
		if ok != c.ok {
			t.Errorf("Route(%q) ok = %v, want %v", c.host, ok, c.ok)
			continue
		}
		if ok && u.Name != c.want {
			t.Errorf("Route(%q) upstream = %q, want %q", c.host, u.Name, c.want)
		}
	}
}

// TestRouteNoAlloc is the whole point of this package: routing on the
// request hot path must not allocate.
func TestRouteNoAlloc(t *testing.T) {
	snap := testSnapshot(t)
	host := []byte("Example.COM:8443")

	var u *Upstream
	var ok bool
	allocs := testing.AllocsPerRun(1000, func() {
		u, ok = snap.Route(host)
	})
	if !ok || u == nil || u.Name != "api" {
		t.Fatalf("Route(%q) = %v, %v; want api/true", host, u, ok)
	}
	if allocs != 0 {
		t.Fatalf("Route allocated %v times per call, want 0", allocs)
	}
}

// TestStoreConcurrentSwap verifies that a reader holding an old *Snapshot
// keeps seeing consistent data while another goroutine concurrently
// publishes new snapshots through the same Store. Run with -race.
func TestStoreConcurrentSwap(t *testing.T) {
	var store Store

	oldSnap, err := NewSnapshot(1,
		[]Route{{Host: "old.example.com", Upstream: "old-up"}},
		[]Upstream{{Name: "old-up", Addrs: []string{"127.0.0.1:9000"}}},
	)
	if err != nil {
		t.Fatalf("NewSnapshot(old): %v", err)
	}
	store.Store(oldSnap)

	// Grab the old snapshot before any concurrent Store happens.
	held := store.Load()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(2); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			newSnap, err := NewSnapshot(i,
				[]Route{{Host: "new.example.com", Upstream: "new-up"}},
				[]Upstream{{Name: "new-up", Addrs: []string{"127.0.0.1:9100"}}},
			)
			if err != nil {
				panic(err)
			}
			store.Store(newSnap)
		}
	}()

	for i := 0; i < 10000; i++ {
		u, ok := held.Route([]byte("old.example.com"))
		if !ok || u.Name != "old-up" {
			t.Fatalf("held snapshot became inconsistent: %v, %v", u, ok)
		}
		if held.Version != 1 {
			t.Fatalf("held snapshot version changed: %d", held.Version)
		}
		if _, ok := held.Route([]byte("new.example.com")); ok {
			t.Fatalf("held snapshot picked up a route from a newer snapshot")
		}
	}
	close(stop)
	wg.Wait()

	latest := store.Load()
	if latest.Version < 2 {
		t.Fatalf("Store did not publish new snapshots: latest version = %d", latest.Version)
	}
}

func TestNewSnapshotValidation(t *testing.T) {
	tests := []struct {
		name      string
		routes    []Route
		upstreams []Upstream
		wantErr   string
	}{
		{
			name:      "empty upstream name",
			upstreams: []Upstream{{Name: "", Addrs: []string{"127.0.0.1:1"}}},
			wantErr:   "empty name",
		},
		{
			name: "duplicate upstream name",
			upstreams: []Upstream{
				{Name: "a", Addrs: []string{"127.0.0.1:1"}},
				{Name: "a", Addrs: []string{"127.0.0.1:2"}},
			},
			wantErr: "duplicate upstream name",
		},
		{
			name:      "empty upstream address list",
			upstreams: []Upstream{{Name: "a", Addrs: nil}},
			wantErr:   "no addresses",
		},
		{
			name:      "malformed upstream address",
			upstreams: []Upstream{{Name: "a", Addrs: []string{"not-a-host-port"}}},
			wantErr:   "invalid address",
		},
		{
			name:      "empty route host",
			routes:    []Route{{Host: "", Upstream: "a"}},
			upstreams: []Upstream{{Name: "a", Addrs: []string{"127.0.0.1:1"}}},
			wantErr:   "empty host",
		},
		{
			name:      "route host with port",
			routes:    []Route{{Host: "example.com:80", Upstream: "a"}},
			upstreams: []Upstream{{Name: "a", Addrs: []string{"127.0.0.1:1"}}},
			wantErr:   "must not include a port",
		},
		{
			name: "duplicate route host",
			routes: []Route{
				{Host: "example.com", Upstream: "a"},
				{Host: "EXAMPLE.com", Upstream: "a"},
			},
			upstreams: []Upstream{{Name: "a", Addrs: []string{"127.0.0.1:1"}}},
			wantErr:   "duplicate route host",
		},
		{
			name:      "unknown upstream referenced by route",
			routes:    []Route{{Host: "example.com", Upstream: "missing"}},
			upstreams: []Upstream{{Name: "a", Addrs: []string{"127.0.0.1:1"}}},
			wantErr:   "unknown upstream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSnapshot(1, tt.routes, tt.upstreams)
			if err == nil {
				t.Fatalf("NewSnapshot() = nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewSnapshot() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	const doc = `{
		"version": 3,
		"upstreams": [{"name": "api", "addrs": ["127.0.0.1:9000"]}],
		"routes": [{"host": "example.com", "upstream": "api"}]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snap, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap.Version != 3 {
		t.Fatalf("Version = %d, want 3", snap.Version)
	}
	u, ok := snap.Route([]byte("example.com"))
	if !ok || u.Name != "api" {
		t.Fatalf("Route = %v, %v, want api/true", u, ok)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/does/not/exist.json"); err == nil {
		t.Fatal("Load() = nil error, want error for missing file")
	}
}

// TestNewSnapshotCopiesItsInputs pins the invariant that a published Snapshot
// is immutable, as a property of the code rather than of caller discipline.
//
// byHost holds pointers into Snapshot.Upstreams, and a published Snapshot is
// read without synchronisation by every in-flight request on every core. If
// NewSnapshot retained the caller's slices, a caller that reused or mutated one
// would be changing live data underneath all of them, and the symptom would be
// a request routed to the wrong upstream rather than a crash.
func TestNewSnapshotCopiesItsInputs(t *testing.T) {
	routes := []Route{{Host: "a.test", Upstream: "one"}}
	ups := []Upstream{{Name: "one", Addrs: []string{"127.0.0.1:1"}}}

	snap, err := NewSnapshot(1, routes, ups)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate everything the caller still holds a reference to.
	routes[0].Host = "mutated.test"
	routes[0].Upstream = "gone"
	ups[0].Name = "mutated"
	ups[0].Addrs[0] = "127.0.0.1:9999"

	u, ok := snap.Route([]byte("a.test"))
	if !ok {
		t.Fatal("route lookup broke after the caller mutated its own slice")
	}
	if u.Name != "one" {
		t.Errorf("upstream name is %q after caller mutation, want %q", u.Name, "one")
	}
	if u.Addrs[0] != "127.0.0.1:1" {
		t.Errorf("upstream address is %q after caller mutation, want %q", u.Addrs[0], "127.0.0.1:1")
	}
	if snap.Routes[0].Host != "a.test" {
		t.Errorf("snapshot route host is %q after caller mutation", snap.Routes[0].Host)
	}
}
