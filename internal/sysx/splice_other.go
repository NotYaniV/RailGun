//go:build !linux

package sysx

// ProbeSplice on a non-Linux host reports splice as unavailable, because it is.
//
// This is not only a build convenience. Development happens on darwin, where
// io.Copy silently falls back to its own 32 KiB buffer: the proxy still works,
// the bytes still move, and a copied-bytes counter that only tracks Railgun's
// own buffers still reads low — which looks exactly like splice evidence. The
// startup line exists so a darwin run says so out loud rather than being
// mistaken for a passing splice arm.
func ProbeSplice(pipes int) SpliceProbe {
	if pipes < 1 {
		pipes = 1
	}
	return SpliceProbe{
		Reason:       "splice(2) is Linux-only; io.Copy will use a userspace buffer",
		WantPipeSize: SpliceWantPipeSize,
		PipesProbed:  pipes,
		MaxSize:      -1,
		SoftPages:    -1,
	}
}
