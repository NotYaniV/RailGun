package proxy

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
	"github.com/NotYaniV/RailGun/internal/httpx"
	"github.com/NotYaniV/RailGun/internal/metrics"
	"github.com/NotYaniV/RailGun/internal/upstream"
)

// A note on the userspace-bytes-copied counter, because the number it produces
// is only interpretable if you know what it excludes.
//
// metrics.AddBytesCopied is called for bytes this proxy moves through its OWN
// buffers: prebuffered body bytes, chunked relay, and the force-copy arm. It
// is NOT called for bytes io.Copy moves internally when splice is unavailable.
// On Linux that distinction is exactly what experiment E2 wants — a low
// counter against a high transfer volume means splice carried the body. On
// darwin, where splice does not exist, io.Copy falls back to its own 32 KiB
// buffer and those bytes go uncounted, so the counter reads low for the same
// reason it would if splice were working. Development numbers from a Mac are
// therefore not splice evidence, and the harness cross-checks against
// perf stat -e syscalls:sys_enter_splice on the rig rather than trusting this
// counter alone.

// connState is the per-connection scratch: buffers and parsed-message storage,
// pooled so that a connection's whole lifetime costs one allocation of each.
//
// The parsed Request and Response hold []byte slices aliasing buf and ubuf, so
// the buffers must not be reused or compacted while a message is still being
// read from them. Every compaction point below is placed after the last read
// of the message it would invalidate.
type connState struct {
	buf []byte // downstream read buffer
	n   int    // bytes valid in buf

	ubuf []byte // upstream read buffer
	un   int    // bytes valid in ubuf

	// reqOut and respOut are separate on purpose, and the reason is a bug that
	// a single shared buffer actually has.
	//
	// The request head must survive until the last upstream attempt, because a
	// retry cannot rebuild it: by then the request body has been forwarded and
	// the read buffer compacted, so st.req no longer aliases valid bytes. If
	// the response head were serialized into the same buffer, a downstream
	// write that failed with zero bytes written would leave wroteResponse
	// false — retry still permitted — while the buffer now held the RESPONSE
	// head. The retry would then send that response head upstream as a
	// request. Two buffers cost a few KiB per pooled connection and make the
	// whole failure mode unrepresentable.
	reqOut  []byte // serialized request head, must outlive every attempt
	respOut []byte // serialized response head
	cpy     []byte // userspace body copy buffer

	req  httpx.Request
	resp httpx.Response
	cs   httpx.ChunkScanner

	// wroteResponse records whether any response byte has reached the client
	// during this exchange. It gates the upstream retry.
	wroteResponse bool

	// methodBuf is a copy of the request method, and it is not a convenience.
	//
	// req.Method aliases buf. The moment the buffer is compacted — which
	// happens as soon as the head has been forwarded, so that a pipelined
	// request behind it survives — every []byte field of req points at bytes
	// that have been overwritten. The method is the one field still needed
	// after that point, by Response.Framing (a HEAD response declares a body
	// it does not send) and by the retry's idempotency check. Sixteen bytes
	// copied once per request is the whole cost of not having that bug.
	methodBuf [16]byte
	methodLen int

	// hadBody records whether the request carried a body, sampled before the
	// buffer is compacted.
	hadBody bool

	// forceCloseDown makes the response announce Connection: close regardless
	// of what either peer asked for. Closing the connection without saying so
	// in the head is not enough: the client is entitled to send another request
	// on a connection it was told would stay open, and it then loses whatever
	// it sent into a socket the proxy is already tearing down.
	forceCloseDown bool
}

// method returns the saved request method.
func (st *connState) method() []byte { return st.methodBuf[:st.methodLen] }

// saveMethod copies the method out of the read buffer. A method longer than
// the buffer is truncated to zero rather than partially copied, which makes it
// match nothing and therefore be treated as non-idempotent and non-HEAD — the
// safe reading in both places the value is used.
func (st *connState) saveMethod(m []byte) {
	if len(m) > len(st.methodBuf) {
		st.methodLen = 0
		return
	}
	st.methodLen = copy(st.methodBuf[:], m)
}

func newConnState(o *Options) *connState {
	return &connState{
		buf:     make([]byte, o.ReadBufferSize),
		ubuf:    make([]byte, o.UpstreamBufferSize),
		reqOut:  make([]byte, 0, o.ReadBufferSize),
		respOut: make([]byte, 0, o.UpstreamBufferSize),
		cpy:     make([]byte, o.CopyBufferSize),
	}
}

func (st *connState) reset() {
	st.n = 0
	st.un = 0
	st.reqOut = st.reqOut[:0]
	st.respOut = st.respOut[:0]
	st.req.Reset()
	st.resp.Reset()
	st.cs.Reset()
	st.wroteResponse = false
	st.methodLen = 0
	st.hadBody = false
	st.forceCloseDown = false
}

// clientError marks a relay failure caused by the DOWNSTREAM client rather than
// by the upstream.
//
// The distinction is not cosmetic here, it is benchmark integrity. roundTrip
// attributed every error out of forward to the upstream, so a client that
// truncated its own request body was counted as ErrBadGateway and answered 502.
// Upstream error rate is a headline number in the comparison against NGINX, and
// a client-caused error booked as an upstream failure is exactly the kind of
// thing that gets noticed in a writeup rather than in a test.
//
// Only unambiguous client faults are wrapped. A failure inside io.Copy between
// the two sockets could be either side and is left attributed to the upstream,
// which is the conservative reading — it over-reports our own error rate rather
// than under-reporting it, and erring toward making our own numbers look worse
// is the only safe direction for a project whose output is a comparison.
type clientError struct{ err error }

func (e *clientError) Error() string { return "proxy: client: " + e.err.Error() }
func (e *clientError) Unwrap() error { return e.err }

func clientFault(err error) error {
	if err == nil {
		return nil
	}
	return &clientError{err: err}
}

// isClientFault reports whether err was attributed to the downstream client.
func isClientFault(err error) bool {
	var ce *clientError
	return errors.As(err, &ce)
}

// exchange drives one request/response pair and reports whether the downstream
// connection may be reused.
func (s *Server) exchange(down *net.TCPConn, st *connState) (bool, error) {
	sample := metrics.ShouldSample()
	var start time.Time
	if sample {
		// The timestamp is taken only when the sample will actually be
		// recorded. Spec 5.8 makes this load-bearing: two time.Now calls cost
		// 40-60ns, which at the request rates being targeted is a measurable
		// fraction of the very thing being measured. Taking a timestamp and
		// then discarding it would defeat the sampling entirely.
		start = time.Now()
	}

	// One deadline for the whole downstream side of the exchange, refreshed
	// per phase inside readRequestHead rather than per syscall.
	if err := s.readRequestHead(down, st); err != nil {
		if errors.Is(err, errClosed) || st.n == 0 {
			// Either the client closed cleanly, or nothing of a request ever
			// arrived — an idle keepalive connection reaching its timeout is
			// the common case. Answering 400 to a client that said nothing
			// would turn ordinary connection reaping into a visible error on
			// both sides, and would inflate the error count on exactly the
			// long-keepalive workloads the benchmark runs.
			return false, err
		}
		status := httpx.StatusFor(err)
		metrics.IncError(metrics.ErrOther)
		s.writeError(down, status)
		return false, err
	}
	metrics.IncRequest()

	// The head is in; bound the remainder of the exchange as a whole.
	//
	// Known limitation, stated rather than solved: this bound is a constant
	// and takes no account of how many body bytes were promised. A large W4
	// response to a genuinely slow client is held to the same deadline as a
	// 1 KiB one. That is fine on the rig, where the load generator is never
	// the slow party, and it would need a byte-rate policy rather than a
	// bigger constant before this faced a real client population.
	if d := s.opts.ReadTimeout + s.opts.WriteTimeout; d > 0 {
		_ = down.SetDeadline(s.clock.Now().Add(d))
	}

	snap := s.cfg.Load()
	up, ok := snap.Route(st.req.HostNoPort())
	if !ok {
		s.writeError(down, 502)
		metrics.IncError(metrics.ErrBadGateway)
		return false, errors.New("proxy: no route for host")
	}

	// Load balancing across upstream members is passive in v0: take the first
	// member whose pool is not in dial backoff. Anything smarter is a separate
	// experiment and would confound the ones already scheduled.
	keepAlive, err := s.roundTrip(down, st, up)

	if sample {
		metrics.RecordLatency(time.Since(start))
	}
	return keepAlive, err
}

// roundTrip acquires an upstream connection, forwards the request, and relays
// the response.
//
// The retry is deliberately narrow. A failure on a REUSED connection before
// any response byte has arrived is almost always the keepalive race: the
// origin closed a connection that was already sitting in the pool. That is
// retriable on a fresh connection and NGINX retries it by default. A failure
// on a FRESH connection is a real upstream failure and must surface. Retrying
// a non-idempotent method is never safe regardless, because the origin may
// have applied it before the connection broke.
func (s *Server) roundTrip(down *net.TCPConn, st *connState, up *config.Upstream) (bool, error) {
	addr, pool, err := s.pickMember(up)
	if err != nil {
		s.writeError(down, 503)
		metrics.IncError(metrics.ErrUpstreamDial)
		return false, err
	}
	_ = addr

	// Serialize the head ONCE, before the first attempt. It cannot be rebuilt
	// on a retry: by then the request body has been forwarded and the read
	// buffer compacted, so st.req no longer points at valid bytes.
	//
	// Serializing from the parsed semantics rather than copying the original
	// bytes is also what closes the smuggling gap. The parse rejected every
	// ambiguous framing, so these bytes say exactly what this proxy concluded
	// the request meant, leaving the origin no room to conclude otherwise.
	st.reqOut = st.reqOut[:0]
	st.reqOut = httpx.AppendRequestHead(st.reqOut, &st.req, false)

	// Drop the head from the read buffer ONCE, here, not once per attempt
	// inside forward. What sits behind the head is a pipelined request, so
	// compacting again on a retry would discard its first HeadLen bytes and
	// the parser would resynchronise partway into a client-controlled message.
	// The retry replays the request; it must not replay the compaction.
	//
	// Safe at this point because reqOut is already serialized: every field of
	// st.req still read after this — HasBody, Close, HeadLen — is a scalar, and
	// the method, the one aliasing field still needed, was copied out by
	// saveMethod.
	if !st.req.HasBody() {
		st.compactAfterHead()
	}

	for attempt := 0; ; attempt++ {
		tok, reused, err := pool.Acquire()
		if err != nil {
			s.writeError(down, 502)
			metrics.IncError(metrics.ErrUpstreamDial)
			return false, err
		}
		if reused {
			metrics.IncPoolHit()
		} else {
			metrics.IncPoolMiss()
		}

		uc := s.slab.get(tok)
		if uc == nil {
			// The token outlived its connection. Treat it as a dead pooled
			// entry rather than trusting it.
			pool.Discard(tok)
			if attempt == 0 && reused {
				continue
			}
			s.writeError(down, 502)
			metrics.IncError(metrics.ErrBadGateway)
			return false, errors.New("proxy: stale upstream token")
		}

		keepAlive, upReusable, err := s.forward(down, uc, st)
		if err != nil {
			pool.Discard(tok)
			if attempt == 0 && reused && !st.wroteResponse && !st.hadBody && idempotent(st.method()) {
				// Keepalive race on a pooled connection. Retry once on a
				// fresh one; the downstream client never learns it happened.
				//
				// Requests with a body are excluded even when the method is
				// idempotent. The body was streamed straight through to the
				// upstream and never buffered, so there is nothing left to
				// replay. Buffering it to enable the retry would mean holding
				// an arbitrary client-controlled amount of memory per
				// connection to salvage a rare race, which is the wrong trade
				// in a proxy whose memory budget is already the declared
				// sacrifice.
				st.resp.Reset()
				st.cs.Reset()
				st.un = 0
				continue
			}
			if isClientFault(err) {
				// The client broke its own request. Answering 502 would blame
				// the origin, and counting it as an upstream error would put a
				// client fault into the number this project publishes.
				if !st.wroteResponse {
					s.writeError(down, 400)
				}
				metrics.IncError(metrics.ErrOther)
				return false, err
			}
			if !st.wroteResponse {
				s.writeError(down, 502)
			}
			metrics.IncError(metrics.ErrBadGateway)
			return false, err
		}

		if upReusable {
			pool.Release(tok)
		} else {
			pool.Discard(tok)
		}
		return keepAlive, nil
	}
}

// pickMember chooses an upstream member.
//
// v0 policy, per spec 5.4: passive health only. Take the first member not in
// dial backoff; if every member is down, take the first anyway so a stale
// backoff cannot turn a recovered upstream into a permanent outage. Weighted
// or least-connections balancing is a separate experiment and would confound
// the ones already scheduled.
func (s *Server) pickMember(up *config.Upstream) (string, *upstream.Pool, error) {
	if len(up.Addrs) == 0 {
		return "", nil, errors.New("proxy: upstream has no members")
	}
	if len(up.Addrs) == 1 {
		// Single-member upstream: there is nothing to choose between, so the
		// health check below decides nothing and is pure cost.
		//
		// It is not free cost. RetryAt takes the pool's mutex, so the loop
		// underneath was acquiring that lock once per request on top of the
		// Acquire and Release that actually do work. A mutex profile of the
		// running proxy put RetryAt at very nearly the same contention delay
		// as Acquire itself — a third of the pool's entire share, spent on a
		// question with one possible answer.
		//
		// Acquire still returns ErrUpstreamDown while the pool is in backoff,
		// so the down state is not being ignored here, only consulted once
		// instead of twice.
		addr := up.Addrs[0]
		return addr, s.pool(addr), nil
	}
	now := s.clock.Now()
	for _, addr := range up.Addrs {
		p := s.pool(addr)
		if p.RetryAt().After(now) {
			continue
		}
		return addr, p, nil
	}
	addr := up.Addrs[0]
	return addr, s.pool(addr), nil
}

// forward writes the request to the upstream and relays the response back.
//
// It returns whether the DOWNSTREAM connection may be reused and whether the
// UPSTREAM connection may be returned to the pool. Those are separate answers:
// a response with Connection: close from the origin makes the upstream
// unreusable while the client connection is still perfectly good, and a
// response whose body is delimited by connection close makes the upstream
// unreusable by definition.
func (s *Server) forward(down, up *net.TCPConn, st *connState) (keepAlive, upReusable bool, err error) {
	if d := s.opts.UpstreamTimeout; d > 0 {
		_ = up.SetDeadline(s.clock.Now().Add(d))
	}

	// st.reqOut was serialized by roundTrip before the first attempt; see the
	// comment there for why it is not rebuilt here.
	if _, err := up.Write(st.reqOut); err != nil {
		return false, false, err
	}

	if st.req.HasBody() {
		if st.req.Expect100 {
			// A client sending Expect: 100-continue is entitled to withhold its
			// body until it sees an interim response, so blocking on that body
			// first deadlocks the exchange: the client waits for 100, the proxy
			// waits for bytes the client is not yet allowed to send, and the
			// only thing that resolves it is a timeout. httpx has always parsed
			// Expect100; nothing read it.
			final, err := s.relayExpectInterim(down, up, st)
			if err != nil {
				return false, false, err
			}
			if final {
				// The origin answered with a final status instead of 100 — 417,
				// or an auth rejection that costs nothing to make before the
				// body arrives. Forwarding the body now would be pointless, and
				// the client may still send it, so those bytes would land in the
				// read buffer and be parsed as the next request. Relay the
				// response, and tell the client the connection is ending so
				// it does not send that body into a socket being torn down.
				st.forceCloseDown = true
				return s.relayResponse(down, up, st)
			}
		}
		if err := s.forwardRequestBody(down, up, st); err != nil {
			return false, false, err
		}
	}

	return s.relayResponse(down, up, st)
}

// relayExpectInterim waits for the origin's verdict on an Expect: 100-continue
// request and forwards an interim response downstream, so the client learns it
// may send its body.
//
// final reports that the origin replied with a final status rather than an
// interim one. In that case the response head is deliberately left in the
// upstream buffer for relayResponse to parse, rather than being handled twice.
//
// Known limitation: this waits on the origin, bounded only by UpstreamTimeout.
// RFC 9110 requires a server receiving Expect: 100-continue to answer with
// either an interim or a final status, so a conforming origin always replies —
// but an origin that ignores Expect and simply waits for the body stalls until
// that timeout. Bounding the wait separately and sending the body anyway is the
// fix if a real origin ever behaves that way.
func (s *Server) relayExpectInterim(down, up *net.TCPConn, st *connState) (final bool, err error) {
	if err := s.readResponseHead(up, st); err != nil {
		return false, err
	}
	if !st.resp.Informational() {
		return true, nil
	}

	st.respOut = st.respOut[:0]
	st.respOut = httpx.AppendTunnelResponseHead(st.respOut, &st.resp)
	if _, err := s.writeDown(down, st.respOut, st); err != nil {
		return false, err
	}
	st.consumeUp(st.resp.HeadLen)
	return false, nil
}

// forwardRequestBody moves the request body upstream.
//
// Bytes of the body that already arrived in the read buffer alongside the head
// are written directly; only the remainder is read from the socket. Skipping
// that step would mean either losing those bytes or reading them a second time
// from a socket that no longer holds them.
func (s *Server) forwardRequestBody(down, up *net.TCPConn, st *connState) error {
	head := st.req.HeadLen

	if st.req.Chunked {
		st.cs.Reset()
		for {
			n, done, err := st.cs.Scan(st.buf[head:st.n])
			if err != nil {
				// Malformed chunked framing from the client.
				return clientFault(err)
			}
			if n > 0 {
				if _, err := up.Write(st.buf[head : head+n]); err != nil {
					return err
				}
				metrics.AddBytesCopied(uint64(n))
			}
			if done {
				st.consume(head + n)
				return nil
			}
			// Discarding the buffer here is correct only because of
			// ChunkScanner's contract: when done is false it has consumed the
			// WHOLE slice, so n == len(p) and every byte has already been
			// forwarded. If it ever returned a short count with done false,
			// this would silently drop body bytes. Stated here because the
			// contract lives in another package and a reviewer had to go read
			// it to convince themselves this was not a bug.
			st.n = 0
			head = 0
			if err := st.fillDown(down); err != nil {
				// The client stopped sending its own body.
				return clientFault(err)
			}
		}
	}

	remaining := st.req.ContentLength
	buffered := int64(st.n - head)
	if buffered > remaining {
		buffered = remaining
	}
	if buffered > 0 {
		if _, err := up.Write(st.buf[head : head+int(buffered)]); err != nil {
			return err
		}
		metrics.AddBytesCopied(uint64(buffered))
		remaining -= buffered
	}
	st.consume(head + int(buffered))

	if remaining > 0 {
		// A request body large enough to matter goes kernel-to-kernel. The
		// LimitedReader is unwrapped by net.spliceFrom, so this still splices.
		//
		// The byte count is checked, and that is not defensive padding. io.Copy
		// over a LimitReader returns a NIL error when the source hits EOF early:
		// it moved everything there was, which is not everything that was
		// promised. Treating that as success sends the origin a head declaring
		// a body it will never finish receiving, and the proxy then waits for a
		// response that cannot come until UpstreamTimeout — one packet from a
		// client pinning an upstream connection and a slab slot for the whole
		// window, and the slab is fixed size.
		n, err := io.Copy(up, io.LimitReader(down, remaining))
		if err != nil {
			return err
		}
		if n < remaining {
			// The client promised more body than it sent.
			return clientFault(io.ErrUnexpectedEOF)
		}
	}
	return nil
}

// relayResponse reads the response head from the upstream and relays head and
// body downstream.
func (s *Server) relayResponse(down, up *net.TCPConn, st *connState) (keepAlive, upReusable bool, err error) {
	for {
		if err := s.readResponseHead(up, st); err != nil {
			return false, false, err
		}

		framing, length := st.resp.Framing(st.method())

		if st.resp.Informational() && framing != httpx.BodyTunnel {
			// A 1xx interim response (typically 100 Continue for an
			// Expect: 100-continue request) is forwarded and then the real
			// response is read from the same connection.
			st.respOut = st.respOut[:0]
			st.respOut = httpx.AppendTunnelResponseHead(st.respOut, &st.resp)
			if _, err := s.writeDown(down, st.respOut, st); err != nil {
				return false, false, err
			}
			st.consumeUp(st.resp.HeadLen)
			continue
		}

		if framing == httpx.BodyTunnel {
			st.respOut = st.respOut[:0]
			st.respOut = httpx.AppendTunnelResponseHead(st.respOut, &st.resp)
			if _, err := s.writeDown(down, st.respOut, st); err != nil {
				return false, false, err
			}
			st.consumeUp(st.resp.HeadLen)
			err := s.tunnel(down, up, st)
			return false, false, err
		}

		closeUp := st.resp.Close || framing == httpx.BodyUntilClose
		closeDown := st.req.Close || st.resp.Close || st.forceCloseDown

		st.respOut = st.respOut[:0]
		st.respOut = httpx.AppendResponseHead(st.respOut, &st.resp, closeDown)
		if _, err := s.writeDown(down, st.respOut, st); err != nil {
			return false, false, err
		}
		st.consumeUp(st.resp.HeadLen)

		if err := s.relayBody(down, up, st, framing, length); err != nil {
			return false, false, err
		}
		return !closeDown, !closeUp, nil
	}
}

// relayBody moves the response body downstream by the cheapest path that is
// correct for its framing.
func (s *Server) relayBody(down, up *net.TCPConn, st *connState, framing httpx.BodyFraming, length int64) error {
	switch framing {
	case httpx.BodyNone:
		return nil

	case httpx.BodyChunked:
		// Chunked framing must be parsed to find the end of the stream, so a
		// blind splice is impossible: the proxy would not know where the body
		// stopped and the connection could not be reused. v0 therefore copies
		// through a pooled buffer, forwarding the encoding verbatim rather
		// than re-chunking. If W4 shows this dominating, the upgrade is a
		// parse-size-line-then-splice-payload loop, not a bigger buffer.
		st.cs.Reset()
		for {
			n, done, err := st.cs.Scan(st.ubuf[:st.un])
			if err != nil {
				return err
			}
			if n > 0 {
				if _, err := s.writeDown(down, st.ubuf[:n], st); err != nil {
					return err
				}
				metrics.AddBytesCopied(uint64(n))
			}
			if done {
				st.consumeUp(n)
				return nil
			}
			// Same reliance on ChunkScanner's all-or-nothing contract as
			// forwardRequestBody; see the note there.
			st.un = 0
			if err := st.fillUp(up); err != nil {
				return err
			}
		}

	case httpx.BodyUntilClose:
		if st.un > 0 {
			if _, err := s.writeDown(down, st.ubuf[:st.un], st); err != nil {
				return err
			}
			metrics.AddBytesCopied(uint64(st.un))
			st.un = 0
		}
		_, err := io.Copy(down, up)
		return err

	default: // BodyLength
		return s.relayLengthBody(down, up, st, length)
	}
}

// relayLengthBody moves a Content-Length delimited body.
//
// This is the function experiment E2 exists to interrogate. Splice is not
// assumed to win: spec 5.5 predicts a crossover body size below which a
// userspace copy is faster, because pushing bytes through the intermediate
// pipe costs two ring round-trips that a single read/write pair into a warm
// L1-resident buffer does not. So the path is chosen by size against a
// threshold, ForceCopy pins it to the copy arm for the paired A/B, and the
// userspace byte counter distinguishes the two arms after the fact.
//
// The critical fragility is that splice happens only because neither side is
// wrapped. Putting a bufio.Reader around up would silently disable it: the
// proxy would still be correct, just slower, with nothing in the code to say
// so. That is why the harness asserts a splice-count-to-bytes ratio on W4
// rather than merely checking the count is non-zero.
func (s *Server) relayLengthBody(down, up *net.TCPConn, st *connState, length int64) error {
	remaining := length

	// Body bytes that arrived alongside the response head are already in
	// userspace; write them before considering splice for the rest.
	if st.un > 0 {
		n := int64(st.un)
		if n > remaining {
			n = remaining
		}
		if _, err := s.writeDown(down, st.ubuf[:n], st); err != nil {
			return err
		}
		metrics.AddBytesCopied(uint64(n))
		remaining -= n
		st.consumeUp(int(n))
	}
	if remaining == 0 {
		return nil
	}

	if !s.opts.ForceCopy && remaining >= s.opts.SpliceThreshold {
		// Short copy is a failure, not a success. The copy arm below already
		// treats an early EOF as io.ErrUnexpectedEOF; this arm did not, so the
		// two arms of experiment E2 disagreed about correctness rather than
		// only about speed.
		//
		// What the missing check allows: an origin that dies mid-response sends
		// fewer bytes than its Content-Length promised, io.Copy reports nil, the
		// exchange looks successful, and the client keeps a connection whose
		// framing it and the proxy no longer agree on — it is still waiting for
		// the rest of a body, so the NEXT response it reads is consumed as body
		// bytes. Returning an error instead makes relayResponse report the
		// connection unreusable in both directions, which is what forces the
		// client to resynchronise by reconnecting.
		n, err := io.Copy(down, io.LimitReader(up, remaining))
		if err != nil {
			return err
		}
		if n < remaining {
			return io.ErrUnexpectedEOF
		}
		return nil
	}

	for remaining > 0 {
		chunk := int64(len(st.cpy))
		if chunk > remaining {
			chunk = remaining
		}
		n, err := up.Read(st.cpy[:chunk])
		if n > 0 {
			if _, werr := s.writeDown(down, st.cpy[:n], st); werr != nil {
				return werr
			}
			metrics.AddBytesCopied(uint64(n))
			remaining -= int64(n)
		}
		if err != nil {
			if err == io.EOF {
				// A Read may return (n > 0, io.EOF) together. If those bytes
				// completed the body, the exchange succeeded and returning EOF
				// here would have the caller tear down a connection that is
				// fine. *net.TCPConn happens never to do this — it reports EOF
				// on a separate zero-byte read — so this is a latent trap
				// rather than a live bug, and it is closed now because the
				// day this parameter becomes an io.Reader is not the day to
				// rediscover it.
				if remaining > 0 {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
	}
	return nil
}

// tunnel relays an established CONNECT or upgraded connection in both
// directions.
//
// This is the ONLY place a second goroutine is spawned per connection, and
// that restraint is the point of spec 5.2: ordinary HTTP/1.1 is half-duplex
// and needs exactly one. Deadlines are cleared first, because a tunnel may sit
// idle indefinitely by design and the per-request timeouts would tear it down.
func (s *Server) tunnel(down, up *net.TCPConn, st *connState) error {
	_ = up.SetDeadline(time.Time{})
	_ = down.SetDeadline(time.Time{})

	if st.un > 0 {
		if _, err := down.Write(st.ubuf[:st.un]); err != nil {
			return err
		}
		metrics.AddBytesCopied(uint64(st.un))
		st.un = 0
	}
	if st.n > 0 {
		if _, err := up.Write(st.buf[:st.n]); err != nil {
			return err
		}
		metrics.AddBytesCopied(uint64(st.n))
		st.n = 0
	}

	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(up, down)
		_ = up.CloseWrite()
		errc <- err
	}()
	_, err := io.Copy(down, up)
	_ = down.CloseWrite()
	if err == nil {
		err = <-errc
	} else {
		<-errc
	}
	return err
}

// readRequestHead reads until a complete request head is parseable.
//
// Parsing restarts from the front of the buffer on every attempt rather than
// resuming, which sounds wasteful and is not: a head arriving in one segment
// parses once, and the pathological case of a head dribbling in a byte at a
// time is bounded by the buffer size, which is itself the 431 limit. Holding
// resumable parser state across reads would cost more on the common path than
// it saves on the rare one.
func (s *Server) readRequestHead(down *net.TCPConn, st *connState) error {
	st.wroteResponse = false
	st.forceCloseDown = false
	idleArmed, headArmed := false, false
	for {
		if st.n > 0 {
			if _, err := st.req.Parse(st.buf[:st.n]); err == nil {
				st.saveMethod(st.req.Method)
				st.hadBody = st.req.HasBody()
				return nil
			} else if err != httpx.ErrIncomplete {
				return err
			}
			if st.n == len(st.buf) {
				return httpx.ErrHeadTooLarge
			}
		}
		// While no byte of a request has arrived this is the keepalive wait and
		// it gets the idle timeout. The moment any byte arrives the deadline
		// tightens to the read timeout, which is what bounds a client that
		// opens a connection and then stalls partway through a header block.
		//
		// The tightening has to be driven by st.n rather than by "is this the
		// first read", because the stall being bounded happens on the SECOND
		// read and later. Arming only on the first pass left a client that
		// sends one byte holding the connection for the whole idle timeout —
		// typically the keepalive window, so orders of magnitude longer than
		// intended, at the cost of one byte.
		//
		// Each is armed once. Re-arming per read would pay for a deadline reset
		// on every segment of a dribbled head and would also let a client that
		// keeps sending bytes slowly extend the bound indefinitely.
		if st.n == 0 {
			if !idleArmed {
				if d := s.opts.IdleTimeout; d > 0 {
					_ = down.SetDeadline(s.clock.Now().Add(d))
				}
				idleArmed = true
			}
		} else if !headArmed {
			if d := s.opts.ReadTimeout; d > 0 {
				_ = down.SetDeadline(s.clock.Now().Add(d))
			}
			headArmed = true
		}
		if err := st.fillDown(down); err != nil {
			return err
		}
	}
}

// readResponseHead reads until a complete response head is parseable.
func (s *Server) readResponseHead(up *net.TCPConn, st *connState) error {
	for {
		if st.un > 0 {
			if _, err := st.resp.Parse(st.ubuf[:st.un]); err == nil {
				return nil
			} else if err != httpx.ErrIncomplete {
				return err
			}
			if st.un == len(st.ubuf) {
				return httpx.ErrHeadTooLarge
			}
		}
		if err := st.fillUp(up); err != nil {
			return err
		}
	}
}

// fillDown reads more downstream bytes into buf.
//
// It sets no deadline. Deadlines are set once per phase by the caller rather
// than once per read, because SetReadDeadline is not free — it takes the poll
// descriptor's lock and resets a runtime timer — and a body arriving in a
// hundred segments would otherwise pay that a hundred times for a bound that
// never changed. A per-phase deadline is also the stronger guarantee: a client
// dribbling bytes just fast enough to reset a per-read timer can hold a
// connection open indefinitely, which a deadline on the whole exchange denies.
func (st *connState) fillDown(down *net.TCPConn) error {
	if st.n == len(st.buf) {
		return httpx.ErrHeadTooLarge
	}
	n, err := down.Read(st.buf[st.n:])
	st.n += n
	if err != nil {
		if err == io.EOF && n == 0 {
			return errClosed
		}
		if n == 0 {
			return err
		}
	}
	return nil
}

// fillUp reads more upstream bytes into ubuf. As with fillDown, the deadline
// is the caller's business and is set once per exchange.
func (st *connState) fillUp(up *net.TCPConn) error {
	if st.un == len(st.ubuf) {
		return httpx.ErrHeadTooLarge
	}
	n, err := up.Read(st.ubuf[st.un:])
	st.un += n
	if err != nil && n == 0 {
		return err
	}
	return nil
}

// consume drops the first n bytes of the downstream buffer, moving whatever
// follows to the front.
//
// Whatever follows is the start of a pipelined request, so it must survive.
// The move is a memmove of at most one request head and is the price of
// supporting pipelining at all; the alternative, a ring buffer, would cost a
// wraparound check on every parse and complicate every slice the parser hands
// back into two possibly-discontiguous pieces.
func (st *connState) consume(n int) {
	if n <= 0 {
		return
	}
	if n >= st.n {
		st.n = 0
		return
	}
	st.n = copy(st.buf, st.buf[n:st.n])
}

// consumeUp is the upstream-buffer equivalent of consume.
func (st *connState) consumeUp(n int) {
	if n <= 0 {
		return
	}
	if n >= st.un {
		st.un = 0
		return
	}
	st.un = copy(st.ubuf, st.ubuf[n:st.un])
}

// compactAfterHead drops a bodyless request head from the buffer.
//
// It must run only after the head has been serialized upstream, because the
// parsed Request's fields alias the bytes it is about to move.
func (st *connState) compactAfterHead() { st.consume(st.req.HeadLen) }

// writeDown writes to the client and records that a response has begun.
//
// The flag it sets is what makes the upstream retry safe: once any response
// byte has reached the client, the exchange can no longer be replayed on a
// fresh upstream connection, because the client has already seen a status line
// that a retry might contradict.
func (s *Server) writeDown(down *net.TCPConn, p []byte, st *connState) (int, error) {
	n, err := down.Write(p)
	if n > 0 {
		st.wroteResponse = true
	}
	return n, err
}
