package httpx

import "bytes"

// BodyFraming describes how a message body is delimited on the wire.
type BodyFraming uint8

const (
	// BodyNone means the message has no body at all.
	BodyNone BodyFraming = iota
	// BodyLength means the body is exactly ContentLength bytes.
	BodyLength
	// BodyChunked means chunked transfer coding.
	BodyChunked
	// BodyUntilClose means the body ends when the upstream closes the
	// connection. The connection cannot be pooled afterwards.
	BodyUntilClose
	// BodyTunnel means the exchange has become an opaque bidirectional byte
	// stream: an accepted CONNECT or a completed protocol upgrade.
	BodyTunnel
)

// Response is a parsed HTTP/1.1 response head. As with Request, every []byte
// aliases the caller's buffer and nothing is allocated.
type Response struct {
	Minor  uint8
	Status int
	Reason []byte

	hdr  [MaxHeaders]Header
	nhdr int

	ContentLength int64
	Chunked       bool
	Close         bool
	Upgrade       []byte
	HeadLen       int
	connVal       []byte
}

// Headers returns the parsed field lines.
func (r *Response) Headers() []Header { return r.hdr[:r.nhdr] }

// Reset clears the response for reuse.
func (r *Response) Reset() {
	r.Minor = 0
	r.Status = 0
	r.Reason = nil
	r.nhdr = 0
	r.ContentLength = -1
	r.Chunked = false
	r.Close = false
	r.Upgrade = nil
	r.HeadLen = 0
	r.connVal = nil
}

// Get returns the first header value matching name, folded case-insensitively.
func (r *Response) Get(name []byte) []byte {
	for i := 0; i < r.nhdr; i++ {
		if EqualFold(r.hdr[i].Name, name) {
			return r.hdr[i].Value
		}
	}
	return nil
}

// Parse parses a response head from the front of buf and returns the bytes
// consumed. Failures carry status 502, since a malformed upstream response is
// a bad gateway rather than a bad request.
func (r *Response) Parse(buf []byte) (int, error) {
	r.Reset()

	line, n, err := nextLine(buf)
	if err != nil {
		if err == ErrIncomplete && len(buf) >= cap(buf) {
			return 0, ErrHeadTooLarge
		}
		return 0, err
	}
	total := n

	if err := r.parseStatusLine(line); err != nil {
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

// parseStatusLine parses "HTTP/1.x SP status SP [reason]". The reason phrase
// is optional and may be empty, but the space that introduces it is not.
func (r *Response) parseStatusLine(line []byte) error {
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return ErrBadStatusLine
	}
	minor, err := parseVersion(line[:sp1])
	if err != nil {
		return ErrBadStatusLine
	}
	rest := line[sp1+1:]
	if len(rest) < 3 {
		return ErrBadStatusLine
	}
	code := 0
	for i := 0; i < 3; i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			return ErrBadStatusLine
		}
		code = code*10 + int(c-'0')
	}
	if len(rest) > 3 {
		if rest[3] != ' ' {
			return ErrBadStatusLine
		}
		// The reason phrase is the one status-line field that is free-form, so
		// it is the one that must be validated rather than matched. nextLine
		// relies on this call to cover these bytes.
		if err := checkFieldValue(rest[4:]); err != nil {
			return ErrBadStatusLine
		}
		r.Reason = rest[4:]
	}
	r.Minor = minor
	r.Status = code
	return nil
}

// resolveSemantics applies the same framing rules as the request path. The
// checks are not weaker just because the peer is an origin we chose: an origin
// that can be induced to emit two Content-Length headers is precisely how a
// response-side smuggle desynchronizes a pooled upstream connection, and that
// pooled connection is then handed to an unrelated client.
func (r *Response) resolveSemantics() error {
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
				sawTE = true
				if !EqualFold(lastToken(val), tokChunked) {
					return ErrBadTransferEnc
				}
				r.Chunked = true
			}
		}
	}

	if sawCL && sawTE {
		return ErrLenAndTransfer
	}
	if r.Minor == 0 && !keepAlive {
		r.Close = true
	}
	if connUpgrde && len(upgradeVal) > 0 {
		r.Upgrade = upgradeVal
	}
	return nil
}

// Framing determines how this response's body is delimited, given the method
// of the request that produced it. RFC 9112 section 6.3, in order.
//
// The request method is required input, not an optimization: a HEAD response
// carries a Content-Length describing a body that is not actually sent. A
// proxy that trusts the header and waits for those bytes hangs until timeout
// and then poisons the pooled connection.
func (r *Response) Framing(reqMethod []byte) (BodyFraming, int64) {
	if r.Status == 101 && r.Upgrade != nil {
		return BodyTunnel, 0
	}
	if bytes.Equal(reqMethod, methodCONNECT) && r.Status >= 200 && r.Status < 300 {
		return BodyTunnel, 0
	}
	if (r.Status >= 100 && r.Status < 200) || r.Status == 204 || r.Status == 304 {
		return BodyNone, 0
	}
	if bytes.Equal(reqMethod, methodHEAD) {
		return BodyNone, 0
	}
	if r.Chunked {
		return BodyChunked, 0
	}
	if r.ContentLength >= 0 {
		return BodyLength, r.ContentLength
	}
	return BodyUntilClose, 0
}

// Informational reports whether this is a 1xx interim response, which the
// proxy must forward and then continue reading for the real response.
func (r *Response) Informational() bool { return r.Status >= 100 && r.Status < 200 }
