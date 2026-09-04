// Package config provides RailGun's routing configuration as an immutable,
// versioned Snapshot published behind an atomic.Pointer, per design spec
// §5.6.
//
// A Snapshot is validated and has all of its lookup structures resolved once
// at construction (NewSnapshot / Load) and is never mutated after that. A
// connection loads the current Snapshot once per request (Store.Load, a
// plain atomic load) and uses it for the whole request; a reload builds a
// brand new Snapshot and swaps the pointer (Store.Store). In-flight requests
// finish against the old snapshot, new requests see the new one — no lock,
// no lazy rebuild on the hot path.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
)

// maxHostLen bounds the stack buffer Route uses to case-normalize an
// incoming Host header. 253 is the longest a valid DNS name can be
// (RFC 1035 §3.1); anything longer can never match a configured route.
const maxHostLen = 253

// Upstream is a named group of one or more backend addresses.
type Upstream struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"` // one or more upstream host:port members
}

// Route maps an exact request Host to an Upstream by name.
type Route struct {
	Host     string `json:"host"`     // exact host match, lowercase, no port
	Upstream string `json:"upstream"` // Upstream.Name
}

// Snapshot is an immutable, versioned configuration. Build one with
// NewSnapshot or Load. Never mutate a Snapshot after publishing it through a
// Store: it is read concurrently, without synchronization, by every
// in-flight request holding a reference to it.
type Snapshot struct {
	Version   uint64
	Routes    []Route
	Upstreams []Upstream

	// byHost is the resolved route lookup table, built once in NewSnapshot
	// and never mutated afterward. Keys are the lowercase, port-stripped
	// route host; values point into Upstreams.
	byHost map[string]*Upstream
}

// NewSnapshot validates routes and upstreams and resolves the route lookup
// table. All of that work happens here, at construction time — never
// lazily — so Route needs no lock and can run on the hot path.
func NewSnapshot(version uint64, routes []Route, upstreams []Upstream) (*Snapshot, error) {
	s := &Snapshot{
		Version:   version,
		Routes:    routes,
		Upstreams: upstreams,
		byHost:    make(map[string]*Upstream, len(routes)),
	}

	byName := make(map[string]*Upstream, len(upstreams))
	for i := range s.Upstreams {
		u := &s.Upstreams[i]
		if u.Name == "" {
			return nil, fmt.Errorf("config: upstream %d has empty name", i)
		}
		if _, dup := byName[u.Name]; dup {
			return nil, fmt.Errorf("config: duplicate upstream name %q", u.Name)
		}
		if len(u.Addrs) == 0 {
			return nil, fmt.Errorf("config: upstream %q has no addresses", u.Name)
		}
		for _, addr := range u.Addrs {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				return nil, fmt.Errorf("config: upstream %q: invalid address %q: %w", u.Name, addr, err)
			}
		}
		byName[u.Name] = u
	}

	for i, r := range s.Routes {
		if r.Host == "" {
			return nil, fmt.Errorf("config: route %d has empty host", i)
		}
		// A route Host must carry no port (see the Route.Host doc). Reject
		// it here — via the same net.SplitHostPort used to validate
		// upstream addresses — rather than silently stripping it, since a
		// configured "example.com:80" almost certainly indicates a mistake.
		if _, _, err := net.SplitHostPort(r.Host); err == nil {
			return nil, fmt.Errorf("config: route %d: host %q must not include a port", i, r.Host)
		}
		host := strings.ToLower(r.Host)
		if _, dup := s.byHost[host]; dup {
			return nil, fmt.Errorf("config: duplicate route host %q", r.Host)
		}
		u, ok := byName[r.Upstream]
		if !ok {
			return nil, fmt.Errorf("config: route %d: unknown upstream %q", i, r.Upstream)
		}
		s.byHost[host] = u
	}

	return s, nil
}

// fileConfig is the on-disk JSON shape read by Load.
type fileConfig struct {
	Version   uint64     `json:"version"`
	Routes    []Route    `json:"routes"`
	Upstreams []Upstream `json:"upstreams"`
}

// Load reads and validates a JSON config file, returning a Snapshot ready to
// publish through a Store. All validation happens here, at load time, never
// at request time.
func Load(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return NewSnapshot(fc.Version, fc.Routes, fc.Upstreams)
}

// Route looks up the Upstream for an incoming request Host, matched
// case-insensitively per RFC 9110 §7.2 and ignoring any ":port" suffix
// (including bracketed IPv6 literals, e.g. "[::1]:8080").
//
// It must not allocate: host normally points into a connection read buffer
// on the hot path, and this is called once per request. Two normalizing
// strategies were rejected in favor of the one below:
//
//   - Lowercasing host in place would mutate the caller's connection read
//     buffer as a side effect of routing, which is surprising and unsafe if
//     anything downstream (logging, header echoing) still needs the
//     original casing.
//   - Falling back to an allocating slow path only for mixed-case input
//     would make the zero-alloc guarantee depend on what the client sent,
//     which is exactly the kind of thing that's invisible in a benchmark
//     using lowercase hosts and then shows up as an allocation under real
//     traffic.
//
// Instead, the (already port-stripped) host is copied and lowercased into a
// fixed-size stack array sized to the longest legal DNS name, and the map
// lookup uses the `m[string(byteSlice)]` form, which the compiler
// recognizes and evaluates without heap-allocating the string key. The
// array itself never escapes, so the whole lookup is zero-alloc regardless
// of input case — normalizing the map keys at load time (via
// strings.ToLower, off the hot path) is what makes this cheap; the request
// path only ever does a fixed-size copy, never a fallback.
func (s *Snapshot) Route(host []byte) (*Upstream, bool) {
	host = stripPort(host)
	if len(host) == 0 || len(host) > maxHostLen {
		return nil, false
	}

	var buf [maxHostLen]byte
	n := copy(buf[:], host)
	for i := 0; i < n; i++ {
		if c := buf[i]; c >= 'A' && c <= 'Z' {
			buf[i] = c + ('a' - 'A')
		}
	}

	u, ok := s.byHost[string(buf[:n])]
	return u, ok
}

// stripPort removes a trailing ":port" from a Host header value, handling
// bracketed IPv6 literals ("[::1]:8080"). It only slices the input; it
// never allocates.
func stripPort(host []byte) []byte {
	if len(host) == 0 {
		return host
	}
	if host[0] == '[' {
		if end := bytes.IndexByte(host, ']'); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if i := bytes.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// Store holds the currently-published Snapshot behind an atomic.Pointer.
// The zero Store has no Snapshot until the first Store call. Load and Store
// are safe for concurrent use by any number of readers and writers.
type Store struct {
	p atomic.Pointer[Snapshot]
}

// Load returns the current Snapshot. It is a plain atomic load — nothing
// else — so it is cheap enough to call once per request.
func (s *Store) Load() *Snapshot {
	return s.p.Load()
}

// Store publishes snap as the current Snapshot. Existing holders of the
// previous Snapshot are unaffected; they keep reading consistent data from
// it until they drop their reference.
func (s *Store) Store(snap *Snapshot) {
	s.p.Store(snap)
}
