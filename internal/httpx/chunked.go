package httpx

import "bytes"

// maxChunkLine bounds a chunk-size line (including any chunk extensions) and
// each trailer field line. It is a trust-boundary limit, not a tuning knob: a
// peer that never sends a newline would otherwise make the scanner buffer
// without bound.
const maxChunkLine = 1024

// maxChunkSize bounds a single declared chunk size at 1 TiB, well below the
// point where the running hex accumulator could overflow int64.
const maxChunkSize = int64(1) << 40

type chunkState uint8

const (
	csSize chunkState = iota
	csData
	csDataCRLF
	csTrailer
	csDone
)

// ChunkScanner tracks chunked transfer-coding framing across an arbitrary
// sequence of reads, so a proxy can forward the encoded bytes verbatim and
// still know exactly where the body ends.
//
// The scanner does not decode and does not copy the body. That is the point:
// the proxy re-emits the same chunked framing it received, so the only thing
// it needs from the encoding is the position of the terminal chunk. Anything
// more would mean re-chunking, which costs a copy per chunk and buys nothing.
//
// Knowing where the body ends is what makes the connection reusable. A proxy
// that guesses is a proxy that eventually serves one client the tail of
// another client's response.
//
// The zero value is ready to scan a fresh body.
type ChunkScanner struct {
	state  chunkState
	remain int64
	line   [maxChunkLine]byte
	nline  int
	// trailerBytes counts bytes spent on the trailer section so a peer cannot
	// stream trailer lines forever.
	trailerBytes int
}

// Reset returns the scanner to its initial state for reuse.
func (s *ChunkScanner) Reset() {
	s.state = csSize
	s.remain = 0
	s.nline = 0
	s.trailerBytes = 0
}

// Done reports whether the terminal chunk and trailer have been seen.
func (s *ChunkScanner) Done() bool { return s.state == csDone }

// Scan reports how many bytes at the front of p belong to the chunked body.
//
// When done is false, n == len(p): the whole buffer is body and the caller may
// forward all of it. When done is true, p[:n] completes the body and p[n:] is
// the start of the next message on the connection, which the caller must
// retain.
func (s *ChunkScanner) Scan(p []byte) (n int, done bool, err error) {
	for n < len(p) {
		switch s.state {
		case csSize:
			adv, complete, err := s.readLine(p[n:])
			n += adv
			if err != nil {
				return n, false, err
			}
			if !complete {
				return n, false, nil
			}
			size, err := parseChunkSize(s.line[:s.nline])
			s.nline = 0
			if err != nil {
				return n, false, err
			}
			if size == 0 {
				s.state = csTrailer
			} else {
				s.remain = size
				s.state = csData
			}

		case csData:
			avail := int64(len(p) - n)
			if avail > s.remain {
				avail = s.remain
			}
			n += int(avail)
			s.remain -= avail
			if s.remain == 0 {
				s.state = csDataCRLF
			}

		case csDataCRLF:
			// A chunk's data is followed by a bare CRLF. Reuse the line reader
			// so a split across reads is handled, and require the line to be
			// empty: any other content means the declared size was wrong and
			// the stream is no longer trustworthy.
			adv, complete, err := s.readLine(p[n:])
			n += adv
			if err != nil {
				return n, false, err
			}
			if !complete {
				return n, false, nil
			}
			if s.nline != 0 {
				s.nline = 0
				return n, false, ErrBadChunkSize
			}
			s.state = csSize

		case csTrailer:
			adv, complete, err := s.readLine(p[n:])
			n += adv
			s.trailerBytes += adv
			if err != nil {
				return n, false, err
			}
			if !complete {
				if s.trailerBytes > maxChunkLine*8 {
					return n, false, ErrTrailerTooLarge
				}
				return n, false, nil
			}
			if s.nline == 0 {
				s.state = csDone
				return n, true, nil
			}
			if s.trailerBytes > maxChunkLine*8 {
				return n, false, ErrTrailerTooLarge
			}
			// Trailer lines are forwarded verbatim; hold them to the same
			// field-line rules as the head so the origin cannot read one
			// differently.
			if _, err := parseHeaderLine(s.line[:s.nline]); err != nil {
				return n, false, ErrBadChunkSize
			}
			s.nline = 0

		case csDone:
			return n, true, nil
		}
	}
	return n, s.state == csDone, nil
}

// readLine accumulates bytes into s.line until CRLF, returning how much of p
// it consumed and whether a full line is now buffered (in s.line[:s.nline],
// without its CRLF).
func (s *ChunkScanner) readLine(p []byte) (n int, complete bool, err error) {
	i := bytes.IndexByte(p, '\n')
	if i < 0 {
		if s.nline+len(p) > maxChunkLine {
			return 0, false, ErrChunkTooLarge
		}
		s.nline += copy(s.line[s.nline:], p)
		return len(p), false, nil
	}
	if s.nline+i > maxChunkLine {
		return 0, false, ErrChunkTooLarge
	}
	s.nline += copy(s.line[s.nline:], p[:i])
	// Strict CRLF, for the same framing-agreement reason as the message head.
	if s.nline == 0 || s.line[s.nline-1] != '\r' {
		return i + 1, false, ErrBadChunkSize
	}
	s.nline--
	return i + 1, true, nil
}

// parseChunkSize parses a hex chunk size, tolerating chunk extensions after a
// semicolon but not interpreting them.
func parseChunkSize(line []byte) (int64, error) {
	if i := bytes.IndexByte(line, ';'); i >= 0 {
		// Extensions are forwarded verbatim, so they must at least be free of
		// control bytes: a bare CR here ends the line for some origins and not
		// for others, and the two would then frame the body differently.
		if checkFieldValue(line[i+1:]) != nil {
			return 0, ErrBadChunkSize
		}
		line = line[:i]
	}
	// Trailing whitespace only (BWS before an extension). Leading whitespace
	// is not in the grammar, and net/http rejects it.
	j := len(line)
	for j > 0 && isOWS(line[j-1]) {
		j--
	}
	line = line[:j]
	if len(line) == 0 || len(line) > 16 {
		return 0, ErrBadChunkSize
	}
	var v int64
	for i := 0; i < len(line); i++ {
		c := line[i]
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			return 0, ErrBadChunkSize
		}
		v = v<<4 | d
		if v > maxChunkSize {
			return 0, ErrChunkTooLarge
		}
	}
	return v, nil
}
