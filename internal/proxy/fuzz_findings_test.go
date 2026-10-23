package proxy_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// rawOrigin accepts one connection, hands the raw request head to inspect, and
// writes resp verbatim before closing.
func rawOrigin(t *testing.T, resp string, heads chan<- []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		var head []byte
		for {
			line, err := br.ReadSlice('\n')
			if err != nil {
				return
			}
			head = append(head, line...)
			if string(line) == "\r\n" {
				break
			}
		}
		if heads != nil {
			heads <- head
		}
		io.WriteString(c, resp)
	}()
	return ln.Addr().String()
}

// A Content-Length request must reach the origin with exactly one
// Content-Length header. Two identical ones are rejected by strict origins
// (NGINX answers 400), and by this proxy's own parser.
func TestForwardedRequestHasOneContentLength(t *testing.T) {
	heads := make(chan []byte, 1)
	addr := rawOrigin(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", heads)
	r := newRigTo(t, testOpts(), addr)

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "POST / HTTP/1.1\r\nHost: app.test\r\nContent-Length: 5\r\n\r\nhello")

	select {
	case head := <-heads:
		if n := bytes.Count(bytes.ToLower(head), []byte("content-length:")); n != 1 {
			t.Fatalf("origin saw %d Content-Length headers:\n%s", n, head)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin never received the request")
	}
}

// An HTTP/1.1 response with no Content-Length and no chunked coding is
// delimited by the origin closing. Forwarded with "Connection: keep-alive" and
// the client connection held open, the client has no way to find the end of
// the body and waits for the idle timeout.
func TestUntilCloseResponseEndsForTheClient(t *testing.T) {
	addr := rawOrigin(t, "HTTP/1.1 200 OK\r\n\r\nhello", nil)
	r := newRigTo(t, testOpts(), addr)

	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: app.test\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body never ended (Connection: %q): read %q, %v",
			resp.Header.Get("Connection"), body, err)
	}
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
}
