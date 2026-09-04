// Package spliceguard checks that W4's splice() syscalls are actually moving
// the response bodies, not just present.
//
// A naive guard ("splice count > 0") is not enough. Linux caps pipe memory
// per user via fs.pipe-user-pages-soft (default 16384 pages, 64 MiB). Go's
// internal/poll allocates a 1 MiB pipe per splice(2) relay; past roughly 64
// concurrent large-body relays that cap is crossed, the kernel starts handing
// out single-page (4 KiB) pipes instead, and internal/poll's attempt to
// resize them back up via F_SETPIPE_SZ fails and the error is discarded. The
// splice path keeps running — syscalls:sys_enter_splice stays comfortably
// non-zero — but each call now moves 1/256th of the buffer, so throughput
// quietly collapses with nothing logged anywhere (spec §5.5).
//
// So instead of presence, this package checks a ratio: how many splice calls
// it took to move each large-body request, and how many bytes each call
// carried on average. Both are derived from the workload's known body size
// and the observed splice count, since splice(2)'s return value is not
// visible to `perf stat` — only the call count is.
package spliceguard

import "fmt"

// Tolerances are the two views of the same measurement — calls per request
// and bytes per call are algebraic inverses of each other given a fixed body
// size — kept as independent knobs so a caller can loosen one without the
// other, e.g. if a rig's TCP segment sizes are unusually small and calls per
// request runs high for reasons unrelated to pipe sizing.
type Tolerances struct {
	// MaxCallsPerRequest is the largest acceptable splice() calls per
	// large-body request. See DefaultMaxCallsPerRequest for the reasoning
	// behind the default.
	MaxCallsPerRequest float64

	// MinBytesPerSpliceFraction is the smallest acceptable average bytes
	// moved per splice() call, as a fraction of the workload's body size. See
	// DefaultMinBytesPerSpliceFraction for the reasoning behind the default.
	MinBytesPerSpliceFraction float64
}

// DefaultMaxCallsPerRequest: a healthy 1 MiB relay pipe does not move the
// whole body in a single splice() call — TCP delivers in segments and a
// splice() only returns what is immediately available on either end — but it
// comfortably moves it in the single digits to low tens of calls (in and out
// combined) even under conservative assumptions about socket buffer sizes. A
// pipe collapsed to one 4 KiB page instead needs 1048576/4096 = 256 round
// trips just for one direction, i.e. several hundred calls for one request —
// two orders of magnitude above any healthy baseline. 8 sits with comfortable
// margin below that collapse and above realistic multi-segment healthy
// traffic.
const DefaultMaxCallsPerRequest = 8

// DefaultMinBytesPerSpliceFraction is the reciprocal of
// DefaultMaxCallsPerRequest (1/8): the same measurement expressed as bytes
// instead of call count. Kept in sync by convention, not by code, since the
// two are independently tunable (see Tolerances).
const DefaultMinBytesPerSpliceFraction = 1.0 / DefaultMaxCallsPerRequest

// DefaultTolerances returns the package defaults, suitable as flag defaults.
func DefaultTolerances() Tolerances {
	return Tolerances{
		MaxCallsPerRequest:        DefaultMaxCallsPerRequest,
		MinBytesPerSpliceFraction: DefaultMinBytesPerSpliceFraction,
	}
}

// Check verifies that spliceCount is consistent with requests large-body
// requests each carrying bodyBytes over a correctly-sized relay pipe. It
// returns a non-nil error — meant to fail the run, not log a warning — naming
// the suspected cause when the ratio looks wrong, since a bare pair of
// numbers is not a diagnosis at 2am.
//
// forceCopy is true for Railgun's -force-copy control arm, where the body is
// routed through a pooled userspace buffer unconditionally (spec §5.5) and
// zero splice calls is the correct outcome, not a failure. The guard only
// applies to the splice-enabled arm, so forceCopy short-circuits it.
func Check(spliceCount float64, requests uint64, bodyBytes int64, forceCopy bool, tol Tolerances) error {
	if forceCopy {
		return nil
	}
	if requests == 0 {
		return nil // nothing was served; nothing to check the ratio of
	}
	if bodyBytes <= 0 {
		return fmt.Errorf("spliceguard: invalid workload body size %d bytes", bodyBytes)
	}

	if spliceCount == 0 {
		return fmt.Errorf(
			"W4 splice guard: 0 splice() calls observed for %d large-body requests; "+
				"the splice path is not engaged at all — check that neither side of the "+
				"relay is wrapped (e.g. in a bufio.Reader), which silently disables "+
				"io.Copy's splice fast path (spec §5.5)",
			requests)
	}

	callsPerRequest := spliceCount / float64(requests)
	bytesPerSplice := (float64(requests) * float64(bodyBytes)) / spliceCount
	minBytesPerSplice := tol.MinBytesPerSpliceFraction * float64(bodyBytes)

	if callsPerRequest > tol.MaxCallsPerRequest || bytesPerSplice < minBytesPerSplice {
		return fmt.Errorf(
			"W4 splice guard: %.1f splice() calls per large-body request (want <= %.1f), "+
				"averaging %.0f bytes/call for a %d-byte body (want >= %.0f, %.0f%% of body) — "+
				"this many-small-splices-instead-of-one-big-one pattern means the kernel has "+
				"silently downsized the relay pipe, most likely because the per-user cap "+
				"fs.pipe-user-pages-soft (default 16384 pages = 64 MiB) was crossed: once that "+
				"happens new pipes come back as a single 4 KiB page, Go's F_SETPIPE_SZ resize "+
				"fails and internal/poll discards the error, so splice keeps 'working' while "+
				"moving 1/256th of the intended buffer per call and W4 throughput quietly drops",
			callsPerRequest, tol.MaxCallsPerRequest, bytesPerSplice, bodyBytes, minBytesPerSplice,
			tol.MinBytesPerSpliceFraction*100)
	}
	return nil
}
