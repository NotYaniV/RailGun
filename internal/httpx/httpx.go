// Package httpx implements zero-allocation HTTP/1.1 message parsing and
// serialization.
//
// The package deliberately knows nothing about I/O. It takes and returns
// []byte and never touches a net.Conn, a bufio.Reader, a file descriptor, or
// a goroutine. That independence is not a style preference: the io_uring data
// plane in milestone M3 reuses this package verbatim, driven from a
// completion-based state machine rather than a dedicated goroutine, and any
// dependency on the netpoll I/O model would have to be unpicked first.
// import_test.go enforces the rule mechanically so it does not rest on
// reviewer discipline.
//
// Parsed messages hold []byte slices that alias the caller's buffer. Nothing
// here copies, and nothing here allocates on the parse path. The caller owns
// the buffer and must not reuse it while a parsed message still references it.
package httpx

import "bytes"

// MaxHeaders bounds the header array carried inline in Request and Response.
//
// A fixed array rather than a map is the central allocation decision in this
// package. net/http's ReverseProxy builds a map[string][]string per request
// and canonicalizes every key; that alone forfeits the throughput target. At
// n<=64 with the whole array resident in L1, a linear scan with an ASCII
// case-insensitive compare beats hashing outright — there is no hash to
// compute and no pointer chase.
const MaxHeaders = 64

// Error is a parse failure carrying the status code the proxy should return.
// All values are package-level singletons, so returning one never allocates.
type Error struct {
	msg string
	// Status is the HTTP status code to send downstream for this failure.
	Status int
}

func (e *Error) Error() string { return e.msg }

func newErr(msg string, status int) *Error { return &Error{msg: msg, Status: status} }

// ErrIncomplete is not a failure. It reports that the buffer does not yet hold
// a complete message head and the caller should read more bytes and retry.
// It carries status 0 because it never reaches the wire.
var ErrIncomplete = newErr("httpx: incomplete message head", 0)

// Parse failures. Status codes follow RFC 9110.
var (
	ErrBadRequestLine  = newErr("httpx: malformed request line", 400)
	ErrBadStatusLine   = newErr("httpx: malformed status line", 502)
	ErrBadVersion      = newErr("httpx: unsupported HTTP version", 505)
	ErrBadHeaderName   = newErr("httpx: malformed header name", 400)
	ErrBadHeaderValue  = newErr("httpx: malformed header value", 400)
	ErrObsFold         = newErr("httpx: obsolete line folding in header", 400)
	ErrBareCR          = newErr("httpx: bare CR in message head", 400)
	ErrBareLF          = newErr("httpx: bare LF line ending in message head", 400)
	ErrTooManyHeaders  = newErr("httpx: header count exceeds limit", 431)
	ErrHeadTooLarge    = newErr("httpx: message head exceeds buffer", 431)
	ErrBadContentLen   = newErr("httpx: malformed Content-Length", 400)
	ErrDupContentLen   = newErr("httpx: conflicting Content-Length headers", 400)
	ErrBadTransferEnc  = newErr("httpx: unsupported Transfer-Encoding", 501)
	ErrLenAndTransfer  = newErr("httpx: both Content-Length and Transfer-Encoding present", 400)
	ErrMissingHost     = newErr("httpx: HTTP/1.1 request without Host", 400)
	ErrDupHost         = newErr("httpx: multiple Host headers", 400)
	ErrBadChunkSize    = newErr("httpx: malformed chunk size", 400)
	ErrChunkTooLarge   = newErr("httpx: chunk size exceeds limit", 400)
	ErrTrailerTooLarge = newErr("httpx: chunked trailer exceeds limit", 400)
)

// Header is one field line. Both slices alias the caller's buffer.
type Header struct {
	Name  []byte
	Value []byte
}

// tokenChar reports, per byte value, whether the byte is a valid RFC 9110
// token character. A 256-entry table costs one L1 line and removes every
// branch from the name-validation loop.
var tokenChar = buildTokenTable()

func buildTokenTable() (t [256]bool) {
	const special = "!#$%&'*+-.^_`|~"
	for i := 0; i < len(special); i++ {
		t[special[i]] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	return t
}

// tokenBad is the complement of tokenChar as a rejection table: 1 for every
// byte that may not appear in a field name. It exists so name validation can
// use the same branchless accumulator loop as checkFieldValue.
var tokenBad = buildTokenBadTable()

func buildTokenBadTable() (t [256]uint8) {
	for i := 0; i < 256; i++ {
		if !tokenChar[i] {
			t[i] = 1
		}
	}
	return t
}

// allToken reports whether every byte of b is a valid token character.
func allToken(b []byte) bool {
	var a0, a1, a2, a3 uint8
	i := 0
	for ; i+4 <= len(b); i += 4 {
		a0 |= tokenBad[b[i]]
		a1 |= tokenBad[b[i+1]]
		a2 |= tokenBad[b[i+2]]
		a3 |= tokenBad[b[i+3]]
	}
	for ; i < len(b); i++ {
		a0 |= tokenBad[b[i]]
	}
	return a0|a1|a2|a3 == 0
}

// lowerASCII maps a byte to its ASCII-lowercase form. Used by fold so the
// case-insensitive compare is a table lookup rather than a branch per byte.
var lowerASCII = buildLowerTable()

func buildLowerTable() (t [256]byte) {
	for i := 0; i < 256; i++ {
		c := byte(i)
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		t[i] = c
	}
	return t
}

// EqualFold reports whether a and b are equal under ASCII case folding.
//
// This is deliberately not bytes.EqualFold: that function is Unicode-aware and
// decodes runes, which is both wrong for HTTP field names (they are ASCII by
// definition) and considerably slower. The length check first means the common
// mismatch case costs one comparison.
func EqualFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII[a[i]] != lowerASCII[b[i]] {
			return false
		}
	}
	return true
}

// isOWS reports whether c is optional whitespace (SP or HTAB).
func isOWS(c byte) bool { return c == ' ' || c == '\t' }

// trimOWS removes leading and trailing SP/HTAB. It reslices; it never copies.
func trimOWS(b []byte) []byte {
	i := 0
	for i < len(b) && isOWS(b[i]) {
		i++
	}
	j := len(b)
	for j > i && isOWS(b[j-1]) {
		j--
	}
	return b[i:j]
}

// fieldValueBad is a per-byte rejection table for header field values: 1 for
// every byte a field value may not contain, 0 otherwise. Legal bytes are HTAB,
// SP through 0x7E, and obs-text 0x80 through 0xFF.
var fieldValueBad = buildFieldValueTable()

func buildFieldValueTable() (t [256]uint8) {
	for i := 0; i < 256; i++ {
		c := byte(i)
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			t[i] = 1
		}
	}
	return t
}

// checkFieldValue validates a field value: visible ASCII, SP, HTAB, or
// obs-text (0x80-0xFF). It rejects NUL, every other control byte, and DEL.
//
// This is also where bare CR and bare LF inside a header value are caught,
// which is why nextLine no longer rescans for them. See the note on nextLine
// for why that split is safe.
//
// The loop is written branchlessly and with four independent accumulators, and
// that shape is deliberate. The obvious version — compare, branch, return on
// the first bad byte — spends a branch per byte on a predicate that is almost
// always false. Folding the predicate into a table lookup removes the branch
// entirely; splitting the OR across four accumulators removes the loop-carried
// dependency so the four lookups issue in parallel rather than serializing on
// a single register. Measured on a 125-byte six-header GET, this shape plus
// the same treatment of the field-name scan took the parse from 148ns to the
// figure recorded in BenchmarkParseRequest.
//
// The error path re-walks the value to name the specific fault. That costs
// nothing that matters, because a request with a malformed header value is
// about to be rejected rather than served.
func checkFieldValue(b []byte) error {
	var a0, a1, a2, a3 uint8
	i := 0
	for ; i+4 <= len(b); i += 4 {
		a0 |= fieldValueBad[b[i]]
		a1 |= fieldValueBad[b[i+1]]
		a2 |= fieldValueBad[b[i+2]]
		a3 |= fieldValueBad[b[i+3]]
	}
	for ; i < len(b); i++ {
		a0 |= fieldValueBad[b[i]]
	}
	if a0|a1|a2|a3 == 0 {
		return nil
	}
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\r':
			return ErrBareCR
		case '\n':
			return ErrBareLF
		}
	}
	return ErrBadHeaderValue
}

// nextLine splits one CRLF-terminated line off the front of buf.
//
// It enforces strict CRLF framing in both directions: a bare LF ends nothing,
// and a CR not immediately followed by LF is rejected. Both rules exist for
// the same reason. Request smuggling works by getting two hops to disagree
// about where a message ends, so a proxy that is more permissive than its
// upstream is itself the vulnerability. Accepting bare LF while the origin
// requires CRLF (or the reverse) is exactly that disagreement.
//
// Performance note, and the reason this function is shorter than it looks like
// it should be: an earlier version scanned each returned line a second time
// for an embedded bare CR. That doubled the number of IndexByte calls per
// message head, which at five to eight header lines was a measurable share of
// total parse time. The second scan is unnecessary because every byte of every
// line already passes through a validator that rejects control characters:
//
//   - request line: method through tokenChar, target through the >0x20 check,
//     version through an exact eight-byte compare.
//   - status line: version and the three status digits exactly, reason phrase
//     through checkFieldValue.
//   - header line: name through tokenChar, value through checkFieldValue.
//     trimOWS only ever strips SP and HTAB, so a CR adjacent to the value
//     survives into checkFieldValue rather than being trimmed away.
//
// Coverage is therefore total, and a bare CR still produces ErrBareCR. If a
// new field is ever parsed out of a line without running one of those
// validators over it, this reasoning breaks and the scan must come back.
//
// Returns the line without its CRLF, the number of bytes consumed including
// the CRLF, and an error.
func nextLine(buf []byte) (line []byte, n int, err error) {
	i := bytes.IndexByte(buf, '\n')
	if i < 0 {
		if cr := bytes.IndexByte(buf, '\r'); cr >= 0 && cr < len(buf)-1 {
			// A CR sits mid-buffer with a non-LF byte after it. No amount of
			// further reading makes this valid, so fail now rather than
			// waiting for a line terminator that cannot legally arrive.
			return nil, 0, ErrBareCR
		}
		return nil, 0, ErrIncomplete
	}
	if i == 0 || buf[i-1] != '\r' {
		return nil, 0, ErrBareLF
	}
	return buf[:i-1], i + 1, nil
}

// parseContentLength parses a strict decimal Content-Length value.
//
// Strict means: digits only, at least one, no sign, no whitespace (the caller
// has already trimmed OWS), no comma list. A comma list such as "5, 5" is
// rejected even when the elements agree, because tolerating it is a documented
// smuggling vector and there is no legitimate sender that emits it.
func parseContentLength(v []byte) (int64, error) {
	if len(v) == 0 || len(v) > 18 {
		return 0, ErrBadContentLen
	}
	var n int64
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, ErrBadContentLen
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// hasToken reports whether the comma-separated list in v contains tok, matched
// case-insensitively. Used for Connection and Transfer-Encoding. It walks the
// list in place with no allocation and no bytes.Split.
func hasToken(v, tok []byte) bool {
	for len(v) > 0 {
		var item []byte
		if i := bytes.IndexByte(v, ','); i >= 0 {
			item, v = v[:i], v[i+1:]
		} else {
			item, v = v, nil
		}
		if EqualFold(trimOWS(item), tok) {
			return true
		}
	}
	return false
}

// lastToken returns the final element of a comma-separated list, OWS-trimmed.
// Transfer-Encoding framing is determined by the last coding, not by whether
// "chunked" appears somewhere in the list.
func lastToken(v []byte) []byte {
	if i := bytes.LastIndexByte(v, ','); i >= 0 {
		v = v[i+1:]
	}
	return trimOWS(v)
}

// Well-known field names, lowercase. Comparison goes through EqualFold, so
// only the length and the folded bytes matter.
var (
	nameHost              = []byte("host")
	nameContentLength     = []byte("content-length")
	nameTransferEncoding  = []byte("transfer-encoding")
	nameConnection        = []byte("connection")
	nameUpgrade           = []byte("upgrade")
	nameExpect            = []byte("expect")
	nameKeepAlive         = []byte("keep-alive")
	nameProxyConnection   = []byte("proxy-connection")
	nameTE                = []byte("te")
	nameTrailer           = []byte("trailer")
	nameProxyAuthenticate = []byte("proxy-authenticate")
	nameProxyAuthz        = []byte("proxy-authorization")

	tokChunked   = []byte("chunked")
	tokClose     = []byte("close")
	tokKeepAlive = []byte("keep-alive")
	tokUpgrade   = []byte("upgrade")
	tok100Cont   = []byte("100-continue")

	methodHEAD    = []byte("HEAD")
	methodCONNECT = []byte("CONNECT")
)

// isHopByHop reports whether a field name is hop-by-hop and must not be
// forwarded. RFC 9110 section 7.6.1.
func isHopByHop(name []byte) bool {
	switch len(name) {
	case 2:
		return EqualFold(name, nameTE)
	case 7:
		return EqualFold(name, nameTrailer) || EqualFold(name, nameUpgrade)
	case 10:
		return EqualFold(name, nameConnection) || EqualFold(name, nameKeepAlive)
	case 16:
		return EqualFold(name, nameProxyConnection)
	case 17:
		return EqualFold(name, nameTransferEncoding)
	case 18:
		return EqualFold(name, nameProxyAuthenticate)
	case 19:
		return EqualFold(name, nameProxyAuthz)
	}
	return false
}
