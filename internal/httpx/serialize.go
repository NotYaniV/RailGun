package httpx

import "strconv"

// Append* functions write a message head into dst and return the extended
// slice. dst is expected to be a pooled buffer with capacity already reserved;
// when it is, none of these functions allocate.

var (
	crlf      = []byte("\r\n")
	sp        = []byte(" ")
	colonSp   = []byte(": ")
	http11    = []byte("HTTP/1.1")
	connClose = []byte("Connection: close\r\n")
	connKeep  = []byte("Connection: keep-alive\r\n")
	teChunked = []byte("Transfer-Encoding: chunked\r\n")
)

// AppendRequestHead serializes r for transmission to an upstream.
//
// Three things happen here that are not mere copying:
//
//   - The version is normalized to HTTP/1.1. The upstream pool only holds
//     keepalive connections, and HTTP/1.0 has no reliable persistence.
//
//   - Hop-by-hop headers are dropped, including every field named in the
//     request's own Connection header. Forwarding those is how a downstream
//     client reaches through the proxy and manipulates the proxy-to-origin
//     connection.
//
//     There are TWO Connection headers in play and confusing them is the most
//     likely misreading of this function — two separate reviewers have each
//     written a test asserting the wrong one, independently. The CLIENT's
//     Connection header describes the client-to-proxy hop; it is stripped, as
//     is every header it names. The Connection header this function EMITS
//     describes the proxy-to-origin hop, which is a pooled connection whose
//     disposition is the proxy's decision and nothing to do with what the
//     client asked for. So the origin always sees a Connection header, and it
//     is never the client's.
//
//   - Framing headers are re-emitted from the parsed semantics rather than
//     copied. The parse already rejected ambiguous framing, so re-emitting
//     guarantees the bytes on the upstream connection say exactly what this
//     proxy concluded the message meant. Copying the originals would leave
//     room for the origin to reach a different conclusion, which is the whole
//     smuggling class.
//
// close controls the Connection header sent upstream.
func AppendRequestHead(dst []byte, r *Request, close bool) []byte {
	dst = append(dst, r.Method...)
	dst = append(dst, sp...)
	dst = append(dst, r.Target...)
	dst = append(dst, sp...)
	dst = append(dst, http11...)
	dst = append(dst, crlf...)

	for i := 0; i < r.nhdr; i++ {
		h := r.hdr[i]
		if skipForward(h.Name, r.connVal) {
			continue
		}
		dst = appendHeader(dst, h)
	}

	if r.Chunked {
		dst = append(dst, teChunked...)
	} else if r.ContentLength >= 0 {
		dst = appendContentLength(dst, r.ContentLength)
	}

	// An upgrade request is the one case where Connection and Upgrade must be
	// re-emitted rather than dropped.
	//
	// Both are hop-by-hop, and stripping them is correct for every ordinary
	// request: forwarding them is how a client reaches through a proxy and
	// manipulates the proxy-to-origin connection. But an upgrade is precisely a
	// request to change the protocol of the next hop, so removing the two
	// headers that say so leaves the origin an ordinary request. It answers 200
	// instead of 101 and no upgrade can ever occur — which is not a degraded
	// upgrade, it is a silently impossible one.
	//
	// Request.Upgrade is non-nil only when the parse saw BOTH a protocol in
	// Upgrade and "upgrade" listed in Connection, so this cannot be triggered
	// by an Upgrade header alone.
	if r.Upgrade != nil {
		dst = append(dst, "Connection: Upgrade\r\nUpgrade: "...)
		dst = append(dst, r.Upgrade...)
		dst = append(dst, crlf...)
		// Two CRLFs, not one: the first terminates the Upgrade field line, the
		// second is the empty line that ends the head. Every other branch here
		// appends a field line that already carries its own terminator, so the
		// single trailing crlf they share is the empty line alone.
		return append(dst, crlf...)
	}

	if close {
		dst = append(dst, connClose...)
	} else {
		dst = append(dst, connKeep...)
	}
	return append(dst, crlf...)
}

// AppendResponseHead serializes r for transmission downstream, under the same
// rules as AppendRequestHead.
//
// framing and length come from Response.Framing, so a HEAD response keeps its
// Content-Length header (the client is entitled to see it) while the proxy
// itself sends no body.
func AppendResponseHead(dst []byte, r *Response, close bool) []byte {
	dst = append(dst, http11...)
	dst = append(dst, sp...)
	dst = strconv.AppendInt(dst, int64(r.Status), 10)
	dst = append(dst, sp...)
	dst = append(dst, r.Reason...)
	dst = append(dst, crlf...)

	for i := 0; i < r.nhdr; i++ {
		h := r.hdr[i]
		if skipForward(h.Name, r.connVal) {
			continue
		}
		dst = appendHeader(dst, h)
	}

	if r.Chunked {
		dst = append(dst, teChunked...)
	}
	if close {
		dst = append(dst, connClose...)
	} else {
		dst = append(dst, connKeep...)
	}
	return append(dst, crlf...)
}

// AppendTunnelResponseHead serializes an interim or upgrade response verbatim
// apart from hop-by-hop filtering, preserving the Connection: upgrade and
// Upgrade headers that a 101 needs to remain meaningful.
func AppendTunnelResponseHead(dst []byte, r *Response) []byte {
	dst = append(dst, http11...)
	dst = append(dst, sp...)
	dst = strconv.AppendInt(dst, int64(r.Status), 10)
	dst = append(dst, sp...)
	dst = append(dst, r.Reason...)
	dst = append(dst, crlf...)
	for i := 0; i < r.nhdr; i++ {
		dst = appendHeader(dst, r.hdr[i])
	}
	return append(dst, crlf...)
}

func appendHeader(dst []byte, h Header) []byte {
	dst = append(dst, h.Name...)
	dst = append(dst, colonSp...)
	dst = append(dst, h.Value...)
	return append(dst, crlf...)
}

func appendContentLength(dst []byte, n int64) []byte {
	dst = append(dst, "Content-Length: "...)
	dst = strconv.AppendInt(dst, n, 10)
	return append(dst, crlf...)
}

// skipForward reports whether a header must be dropped when relaying: either
// it is hop-by-hop by name, or the peer listed it in its Connection header.
func skipForward(name, connVal []byte) bool {
	if isHopByHop(name) {
		return true
	}
	return len(connVal) > 0 && hasToken(connVal, name)
}

// Canned error responses. These are the only bytes the proxy emits when it
// fails before or instead of reaching an upstream, so they are pre-rendered
// constants: a failure path that allocates is a failure path that gets worse
// under exactly the load that triggers it.
var errorBodies = map[int]string{
	400: "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	408: "HTTP/1.1 408 Request Timeout\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	413: "HTTP/1.1 413 Content Too Large\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	431: "HTTP/1.1 431 Request Header Fields Too Large\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	501: "HTTP/1.1 501 Not Implemented\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	502: "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	503: "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	504: "HTTP/1.1 504 Gateway Timeout\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
	505: "HTTP/1.1 505 HTTP Version Not Supported\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
}

// errorTable is the map above flattened at init into a form the hot path can
// index without hashing.
var errorTable = buildErrorTable()

type errorEntry struct {
	status int
	body   []byte
}

func buildErrorTable() []errorEntry {
	t := make([]errorEntry, 0, len(errorBodies))
	for _, s := range []int{400, 408, 413, 431, 501, 502, 503, 504, 505} {
		t = append(t, errorEntry{status: s, body: []byte(errorBodies[s])})
	}
	return t
}

// ErrorResponse returns the pre-rendered response for a status code, falling
// back to 400 for anything unrecognized. The returned slice is shared and must
// not be modified.
func ErrorResponse(status int) []byte {
	for i := range errorTable {
		if errorTable[i].status == status {
			return errorTable[i].body
		}
	}
	return errorTable[0].body
}

// StatusFor extracts the response status from a parse error, defaulting to 400
// for anything that is not an *Error.
func StatusFor(err error) int {
	if e, ok := err.(*Error); ok && e.Status != 0 {
		return e.Status
	}
	return 400
}
