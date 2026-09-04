package spliceguard

import (
	"strings"
	"testing"
)

const w4BodyBytes = 1048576 // W4's body size, spec §4.4

func TestCheck(t *testing.T) {
	cases := []struct {
		name        string
		spliceCount float64
		requests    uint64
		bodyBytes   int64
		forceCopy   bool
		wantErr     bool
	}{
		{
			name: "healthy: about one splice per request",
			// A handful of calls per request (in+out, multi-segment) is normal.
			spliceCount: 2500,
			requests:    1000,
			bodyBytes:   w4BodyBytes,
			wantErr:     false,
		},
		{
			name: "pipe downsized: high call count, low bytes per call",
			// Pipe collapsed to one 4 KiB page: ~256 round trips per
			// direction to move a 1 MiB body.
			spliceCount: 512000,
			requests:    1000,
			bodyBytes:   w4BodyBytes,
			wantErr:     true,
		},
		{
			name:        "zero splice calls: path not engaged at all",
			spliceCount: 0,
			requests:    1000,
			bodyBytes:   w4BodyBytes,
			wantErr:     true,
		},
		{
			name: "force-copy control arm: zero splices is correct",
			// Same shape as the zero-splice failure case, but this is the
			// -force-copy arm, where the guard must not apply at all.
			spliceCount: 0,
			requests:    1000,
			bodyBytes:   w4BodyBytes,
			forceCopy:   true,
			wantErr:     false,
		},
		{
			name:        "force-copy control arm: even a nonzero count is not checked",
			spliceCount: 999999,
			requests:    1000,
			bodyBytes:   w4BodyBytes,
			forceCopy:   true,
			wantErr:     false,
		},
		{
			name:        "no requests served: nothing to check",
			spliceCount: 0,
			requests:    0,
			bodyBytes:   w4BodyBytes,
			wantErr:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.spliceCount, c.requests, c.bodyBytes, c.forceCopy, DefaultTolerances())
			if (err != nil) != c.wantErr {
				t.Errorf("Check() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// The failure message is read at 2am; it must name the suspected cause
// (pipe downsizing via fs.pipe-user-pages-soft), not just print two numbers.
func TestCheckFailureMessageNamesCause(t *testing.T) {
	err := Check(512000, 1000, w4BodyBytes, false, DefaultTolerances())
	if err == nil {
		t.Fatal("Check() returned nil, want an error")
	}
	got := err.Error()
	for _, want := range []string{"fs.pipe-user-pages-soft", "F_SETPIPE_SZ", "splice() calls per large-body request"} {
		if !strings.Contains(got, want) {
			t.Errorf("error message missing %q: %q", want, got)
		}
	}
}

func TestCheckZeroSpliceMessageNamesCause(t *testing.T) {
	err := Check(0, 1000, w4BodyBytes, false, DefaultTolerances())
	if err == nil {
		t.Fatal("Check() returned nil, want an error")
	}
	got := err.Error()
	for _, want := range []string{"bufio.Reader", "splice"} {
		if !strings.Contains(got, want) {
			t.Errorf("error message missing %q: %q", want, got)
		}
	}
}
