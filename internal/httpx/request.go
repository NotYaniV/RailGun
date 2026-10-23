package httpx

import "bytes"

// Request is a parsed HTTP/1.1 request head.
//
// Every []byte field aliases the buffer passed to Parse. Nothing is copied and
// nothing is allocated, so the request is only valid for as long as that
// buffer is intact and unmodified.
//
// The header array is inline rather than behind a slice header so that a
// Request drawn from a pool is a single object with no second allocation, and
// so that copying a Request cannot leave a slice aliasing the original's
// array. Use Headers() to read them.
type Request struct {
	Method []byte
	Target []byte
	// Minor is the minor version: 1 for HTTP/1.1, 0 for HTTP/1.0.
	Minor uint8

	hdr  [MaxHeaders]Header
	nhdr int

	// Host is the value of the Host header, still aliasing the buffer.
	Host []byte
	// ContentLength is the declared body length, or -1 when absent.
	ContentLength int64
	// Chunked reports Transfer-Encoding: chunked framing.
	Chunked bool
	// Close reports that the connection must not be reused after this
	// exchange, whether because of Connection: close or HTTP/1.0 defaults.
	Close bool
	// Upgrade is the Upgrade header value when the request both names a
	// protocol and lists "upgrade" in Connection. Nil otherwise. A non-nil
	// Upgrade, or a CONNECT method, is what makes an exchange bidirectional
	// and forces the proxy to spend a second goroutine on it.
	Upgrade []byte
	// Expect100 reports Expect: 100-continue.
	Expect100 bool
	// HeadLen is the byte count of the parsed head, including the final CRLF.
	HeadLen int
	// connVal retains the raw Connection header value so serialization can
	// strip the headers it names without a second scan.
	connVal []byte
}

// Headers returns the parsed field lines. The slice aliases the Request's
// inline array and is invalidated by the next Parse.
func (r *Request) Headers() []Header { return r.hdr[:r.nhdr] }

// Reset clears the request for reuse without releasing its array.
func (r *Request) Reset() {
	r.Method = nil
	r.Target = nil
	r.Minor = 0
	r.nhdr = 0
	r.Host = nil
	r.ContentLength = -1
	r.Chunked = false
	r.Close = false
	r.Upgrade = nil
	r.Expect100 = false
	r.HeadLen = 0
	r.connVal = nil
}

// Get returns the first header value matching name, folded case-insensitively.
func (r *Request) Get(name []byte) []byte {
	for i := 0; i < r.nhdr; i++ {
		if EqualFold(r.hdr[i].Name, name) {
			return r.hdr[i].Value
		}
	}
	return nil
}

// Bidirectional reports whether this exchange cannot be driven as a
// half-duplex request-then-response sequence and therefore needs a second
// goroutine relaying the opposite direction.
func (r *Request) Bidirectional() bool {
	return r.Upgrade != nil || bytes.Equal(r.Method, methodCONNECT)
}

// HasBody reports whether a request body follows the head.
func (r *Request) HasBody() bool {
	return r.Chunked || r.ContentLength > 0
}

// Parse parses a request head from the front of buf.
//
// It returns the number of bytes consumed. If buf does not yet contain a
// complete head it returns ErrIncomplete and the caller should read more and
// call again from the start of the same buffer; parsing is restartable and
// holds no state between calls.
//
// All failures are *Error carrying the status code to return downstream.
func (r *Request) Parse(buf []byte) (int, error) {
	r.Reset()

	line, n, err := nextLine(buf)
	if err != nil {
		if err == ErrIncomplete && len(buf) >= cap(buf) {
			return 0, ErrHeadTooLarge
		}
		return 0, err
	}
	total := n

	if err := r.parseRequestLine(line); err != nil {
		return 0, err
	}

	for {
		line, n, err := nextLine(buf[total:])
		if err != nil {
			if err == ErrIncomplete && len(buf) >= cap(buf) {
				return 0, ErrHeadTooLarge
			}
			return 0, err
		}
		total += n

		if len(line) == 0 {
			r.HeadLen = total
			if err := r.resolveSemantics(); err != nil {
				// Report zero bytes consumed on every failure. The caller
				// treats a non-zero count as "this much of the buffer is
				// dealt with"; handing it a count alongside an error would
				// have it advance past a message it never accepted.
				return 0, err
			}
			return total, nil
		}
		if isOWS(line[0]) {
			// Obsolete line folding. RFC 9112 requires a proxy to either
			// reject it or replace it with spaces before forwarding; rejecting
			// is the only option that cannot change the message's meaning.
			return 0, ErrObsFold
		}
		if r.nhdr == MaxHeaders {
			return 0, ErrTooManyHeaders
		}
		h, err := parseHeaderLine(line)
		if err != nil {
			return 0, err
		}
		r.hdr[r.nhdr] = h
		r.nhdr++
	}
}

// parseRequestLine parses "METHOD SP request-target SP HTTP/1.x".
//
// Exactly one SP is required at each separator. Tolerating runs of spaces or
// tabs here is a known smuggling primitive, because the two hops that disagree
// about where the target ends disagree about what was requested.
func (r *Request) parseRequestLine(line []byte) error {
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return ErrBadRequestLine
	}
	method := line[:sp1]
	if !allToken(method) {
		return ErrBadRequestLine
	}

	rest := line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return ErrBadRequestLine
	}
	target := rest[:sp2]
	for i := 0; i < len(target); i++ {
		// The target must be visible ASCII. A space would have terminated it,
		// so anything below 0x21 or at 0x7f is a control character.
		if target[i] <= 0x20 || target[i] == 0x7f {
			return ErrBadRequestLine
		}
	}

	ver := rest[sp2+1:]
	minor, err := parseVersion(ver)
	if err != nil {
		return err
	}

	r.Method = method
	r.Target = target
	r.Minor = minor
	return nil
}

// parseVersion parses "HTTP/1.0" or "HTTP/1.1" and nothing else.
func parseVersion(v []byte) (uint8, error) {
	if len(v) != 8 || string(v[:7]) != "HTTP/1." {
		return 0, ErrBadVersion
	}
	switch v[7] {
	case '0':
		return 0, nil
	case '1':
		return 1, nil
	}
	return 0, ErrBadVersion
}

// parseHeaderLine splits "name: value" with strict name validation.
//
// No whitespace is permitted between the field name and the colon. RFC 9112
// makes that a must-reject for exactly the smuggling reason above: a hop that
// accepts "Content-Length : 5" and one that does not will frame the same
// stream differently.
func parseHeaderLine(line []byte) (Header, error) {
	// One pass, walking to the colon while validating token bytes on the way.
	//
	// The branchless treatment that pays off in checkFieldValue does NOT pay
	// off here, and it was tried: locating the colon with bytes.IndexByte and
	// then validating the name with the same four-accumulator loop measured
	// 140ns against this version's 134ns on BenchmarkParseRequest. The
	// difference is that checkFieldValue's predicate is evaluated over every
	// byte regardless, so removing its branch is free, whereas the colon
	// search is the loop's exit condition — replacing it with IndexByte does
	// not remove a traversal, it adds one. Left as-is deliberately; do not
	// "optimize" it back without a benchmark.
	//
	// Validating each name byte is also what rejects "Content-Length : 6":
	// the space is not a token character, so it fails here rather than being
	// tolerated as padding before the colon. A hop that tolerates it frames
	// the message differently from one that does not.
	c := 0
	for c < len(line) && tokenChar[line[c]] {
		c++
	}
	if c == 0 || c == len(line) || line[c] != ':' {
		return Header{}, ErrBadHeaderName
	}
	value := trimOWS(line[c+1:])
	if err := checkFieldValue(value); err != nil {
		return Header{}, err
	}
	return Header{Name: line[:c], Value: value}, nil
}

// resolveSemantics derives framing and connection disposition from the parsed
// headers, and enforces the message-framing rules that make the proxy safe to
// put in front of an origin server.
//
// The rules that matter, and why each is a rejection rather than a preference:
//
//   - Content-Length together with Transfer-Encoding: the two hops will pick
//     different framing. This is the canonical CL.TE / TE.CL smuggle.
//   - Two Content-Length headers: same disagreement, one hop reads the first
//     and one the last. Rejected even when they agree, because agreeing is not
//     something this code should have to reason about.
//   - Transfer-Encoding other than a single "chunked": a final coding that is
//     not chunked leaves the body no determinable end, and one stacked under
//     chunked would be lost when the header is re-emitted.
//   - HTTP/1.1 without exactly one Host: the routing decision has no input.
func (r *Request) resolveSemantics() error {
	var (
		sawCL      bool
		sawTE      bool
		keepAlive  bool
		connUpgrde bool
		upgradeVal []byte
	)

	for i := 0; i < r.nhdr; i++ {
		name, val := r.hdr[i].Name, r.hdr[i].Value
		switch len(name) {
		case 2:
			if EqualFold(name, nameTE) {
				continue
			}
		case 4:
			if EqualFold(name, nameHost) {
				if r.Host != nil {
					return ErrDupHost
				}
				r.Host = val
			}
		case 6:
			if EqualFold(name, nameExpect) && hasToken(val, tok100Cont) {
				r.Expect100 = true
			}
		case 7:
			if EqualFold(name, nameUpgrade) {
				upgradeVal = val
			}
		case 10:
			if EqualFold(name, nameConnection) {
				r.connVal = val
				if hasToken(val, tokClose) {
					r.Close = true
				}
				if hasToken(val, tokKeepAlive) {
					keepAlive = true
				}
				if hasToken(val, tokUpgrade) {
					connUpgrde = true
				}
			}
		case 14:
			if EqualFold(name, nameContentLength) {
				if sawCL {
					return ErrDupContentLen
				}
				sawCL = true
				n, err := parseContentLength(val)
				if err != nil {
					return err
				}
				r.ContentLength = n
			}
		case 17:
			if EqualFold(name, nameTransferEncoding) {
				// Exactly one Transfer-Encoding, exactly "chunked". Any other
				// coding would be stripped by the serializer (the header is
				// hop-by-hop and re-emitted as plain chunked), handing the
				// peer a body still encoded in a coding it was never told of.
				if sawTE || !EqualFold(val, tokChunked) {
					return ErrBadTransferEnc
				}
				sawTE = true
				r.Chunked = true
			}
		}
	}

	if sawCL && sawTE {
		return ErrLenAndTransfer
	}
	if r.Minor >= 1 && r.Host == nil {
		return ErrMissingHost
	}
	if r.Minor == 0 && !keepAlive {
		// HTTP/1.0 closes by default; only an explicit Connection: keep-alive
		// opts in.
		r.Close = true
	}
	if connUpgrde && len(upgradeVal) > 0 {
		r.Upgrade = upgradeVal
	}
	return nil
}

// HostNoPort returns Host with any ":port" suffix removed, without allocating.
// Route lookup keys on the host alone.
func (r *Request) HostNoPort() []byte {
	h := r.Host
	// Scan from the right so an IPv6 literal's internal colons are not
	// mistaken for the port separator.
	i := bytes.LastIndexByte(h, ':')
	if i < 0 {
		return h
	}
	if j := bytes.LastIndexByte(h, ']'); j > i {
		return h
	}
	return h[:i]
}
