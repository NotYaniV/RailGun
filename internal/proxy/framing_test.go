package proxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// An origin that writes more bytes than its response framing accounted for
// must not have that connection returned to the pool.
//
// The leftover bytes live in the connState's upstream buffer, which belongs to
// the DOWNSTREAM connection, not to the upstream one. So they outlive the
// upstream connection they arrived on: pool the connection and the next
// exchange on this client connection parses those stale bytes as its own
// response — possibly after acquiring an entirely different upstream. The
// client is answered with something the origin never sent for that request.
//
// This is the same class as the truncation bugs, in the opposite direction: too
// few bytes desynchronises the client, too many desynchronises the proxy.
func TestOriginOverrunIsNotPooled(t *testing.T) {
	const injected = "INJECTED"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				first := true
				for {
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					if first {
						first = false
						// A correct response and a whole second response nobody
						// asked for, in ONE write so they arrive together and
						// the proxy has both in its buffer when the exchange
						// ends. Two writes would make this a race against the
						// scheduler rather than a test — see the limitation
						// noted on the fix in relayResponse.
						io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n"+
							"Connection: keep-alive\r\n\r\nFIRST"+
							"HTTP/1.1 200 OK\r\nContent-Length: 8\r\n"+
							"Connection: keep-alive\r\n\r\n"+injected)
						continue
					}
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n"+
						"Connection: keep-alive\r\n\r\nSECOND")
				}
			}(c)
		}
	}()

	r := newRigTo(t, testOpts(), ln.Addr().String())
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)

	req := "GET /a HTTP/1.1\r\nHost: app.test\r\n\r\n"

	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "FIRST" {
		t.Fatalf("first response body %q, want %q", body, "FIRST")
	}

	// Second request on the same client connection. It must not be answered
	// with the response the origin volunteered during the first exchange.
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		// A closed connection here is an acceptable outcome: the proxy refused
		// to reuse state it could not account for.
		return
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) == injected {
		t.Fatal("second request was answered with a response the origin " +
			"volunteered during the FIRST exchange; leftover upstream bytes " +
			"survived into the next exchange")
	}
}
