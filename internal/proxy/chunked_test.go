package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestChunkedRequestBodyAcrossManyReads exercises the multi-iteration path in
// forwardRequestBody's chunked branch.
//
// The existing chunked test hands net/http a body and lets it write however it
// likes, which in practice is one or two segments — so the scan loop runs once
// and the interesting part never executes. Here the client writes each chunk in
// its own TCP segment, and splits several chunk headers away from their data,
// forcing the scanner to resume across reads and the relay to make the buffer
// reset and offset adjustment that only happens on the second pass and later.
//
// Getting that offset wrong does not fail loudly. It forwards a body that is
// subtly not the body the client sent, which is why the assertion is on the
// echoed bytes rather than on the status code.
func TestChunkedRequestBodyAcrossManyReads(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	c, err := net.DialTimeout("tcp", r.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))

	head := "POST /echo HTTP/1.1\r\nHost: app.test\r\nTransfer-Encoding: chunked\r\n\r\n"
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}

	// Varied chunk sizes, including ones that cross the 16-byte and 256-byte
	// boundaries where a size line changes length.
	sizes := []int{1, 5, 17, 255, 256, 257, 4095, 3, 1024, 2}
	var want strings.Builder
	for i, n := range sizes {
		payload := make([]byte, n)
		for j := range payload {
			payload[j] = byte('a' + (i+j)%26)
		}
		want.Write(payload)

		// Split the size line from its data so the scanner has to resume
		// mid-chunk rather than always seeing a whole one.
		if _, err := fmt.Fprintf(c, "%x\r\n", n); err != nil {
			t.Fatalf("chunk %d header: %v", i, err)
		}
		time.Sleep(time.Millisecond)
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("chunk %d data: %v", i, err)
		}
		if _, err := io.WriteString(c, "\r\n"); err != nil {
			t.Fatalf("chunk %d terminator: %v", i, err)
		}
	}
	if _, err := io.WriteString(c, "0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("status %q, want 200", strings.TrimSpace(status))
	}

	var contentLen int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			fmt.Sscanf(strings.TrimSpace(v), "%d", &contentLen)
		}
	}

	if contentLen != want.Len() {
		t.Fatalf("origin echoed %d bytes, client sent %d; the chunked relay lost or duplicated body bytes",
			contentLen, want.Len())
	}
	got := make([]byte, contentLen)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading echoed body: %v", err)
	}
	if string(got) != want.String() {
		for i := range got {
			if got[i] != want.String()[i] {
				t.Fatalf("chunked relay corrupted the body at offset %d of %d", i, len(got))
			}
		}
	}
}

// TestChunkedRequestFollowedByPipelinedRequest checks that the chunked relay
// stops at the terminal chunk and leaves the next request intact.
//
// This is the chunked equivalent of the pipelining defect fixed in 2ed6ee6.
// Consuming one byte too many or too few here does not error — it resynchronises
// the parser partway into a client-controlled message, and whatever survives is
// forwarded as a request the client never sent.
func TestChunkedRequestFollowedByPipelinedRequest(t *testing.T) {
	r := newRig(t, testOpts(), echoHandler())

	c, err := net.DialTimeout("tcp", r.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))

	// A chunked POST and a following GET, written as one buffer so the second
	// request is already sitting behind the first when the body is relayed.
	both := "POST /echo HTTP/1.1\r\nHost: app.test\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n" +
		"GET /small HTTP/1.1\r\nHost: app.test\r\n\r\n"
	if _, err := io.WriteString(c, both); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)

	first := readOneResponse(t, br)
	if !strings.Contains(first, "200") {
		t.Fatalf("first response %q, want 200", first)
	}
	if !strings.Contains(first, "hello world") {
		t.Fatalf("first response body wrong: %q", first)
	}

	second := readOneResponse(t, br)
	if !strings.Contains(second, "200") {
		t.Fatalf("pipelined request after a chunked body got %q; the chunked relay did not stop cleanly at the terminal chunk", second)
	}
	if !strings.Contains(second, "hello") {
		t.Fatalf("pipelined response body wrong: %q", second)
	}
}

// readOneResponse reads a single Content-Length delimited response.
func readOneResponse(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	n := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response head: %v", err)
		}
		sb.WriteString(line)
		if line == "\r\n" {
			break
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		}
	}
	if n > 0 {
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			t.Fatalf("reading response body: %v", err)
		}
		sb.Write(body)
	}
	return sb.String()
}
