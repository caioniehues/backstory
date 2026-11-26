package embed

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// The query server's client is this hand-rolled HTTP/1.1 over a unix
// socket: net/http is banned from shipped code, socket or not
// (ops/nonetwork_test.go), and llama-server speaks HTTP. It sends one
// request per connection (Connection: close) and reads a Content-Length,
// chunked or read-to-close body. That is all llama-server's answers need.

const (
	// unixDialTimeout bounds a dial when ctx has no deadline.
	unixDialTimeout = 5 * time.Second
	// maxResponseBytes caps a response body; a 768-d vector is about 10 KB.
	maxResponseBytes = 1 << 20
)

// unixHTTP sends method target with body (JSON, when not nil) to the HTTP
// server on the unix socket at sock and returns the status code and body.
// It honours ctx: when ctx ends first the error wraps ctx.Err().
func unixHTTP(ctx context.Context, sock, method, target string, body []byte) (int, []byte, error) {
	timeout := unixDialTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
		if timeout <= 0 {
			return 0, nil, ctx.Err()
		}
	}
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return 0, nil, ctxErr(ctx, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A cancelled ctx unblocks a read or write in flight.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var req bytes.Buffer
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n", method, target)
	if body != nil {
		fmt.Fprintf(&req, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req.WriteString("\r\n")
	req.Write(body)
	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, nil, ctxErr(ctx, err)
	}
	status, resp, err := readResponse(bufio.NewReader(conn))
	if err != nil {
		return 0, nil, ctxErr(ctx, err)
	}
	return status, resp, nil
}

// ctxErr is err, or ctx's own error when ctx ended (the deadline or a
// cancel is the cause, not the I/O error it produced). The connection's
// deadline is ctx's own, so its timeout can land just before ctx's timer
// marks ctx done: an I/O timeout under a ctx deadline waits for that.
func ctxErr(ctx context.Context, err error) error {
	if _, ok := ctx.Deadline(); ok && errors.Is(err, os.ErrDeadlineExceeded) {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return fmt.Errorf("query server: %w", ctx.Err())
	}
	return fmt.Errorf("query server: %w", err)
}

// readResponse reads one HTTP/1.1 response: the status line, the headers
// and the body.
func readResponse(r *bufio.Reader) (int, []byte, error) {
	line, err := readLine(r)
	if err != nil {
		return 0, nil, fmt.Errorf("reading the status line: %w", err)
	}
	proto, rest, _ := strings.Cut(line, " ")
	code, _, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(code)
	if !strings.HasPrefix(proto, "HTTP/1.") || err != nil || status < 100 || status > 999 {
		return 0, nil, fmt.Errorf("malformed status line %q", line)
	}
	length, chunked := -1, false
	for {
		h, err := readLine(r)
		if err != nil {
			return 0, nil, fmt.Errorf("reading the headers: %w", err)
		}
		if h == "" {
			break
		}
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return 0, nil, fmt.Errorf("malformed header %q", h)
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			if length, err = strconv.Atoi(value); err != nil || length < 0 {
				return 0, nil, fmt.Errorf("malformed Content-Length %q", value)
			}
		case "transfer-encoding":
			chunked = strings.EqualFold(value, "chunked")
		}
	}
	var body []byte
	switch {
	case chunked:
		body, err = readChunked(r)
	case length > maxResponseBytes:
		err = fmt.Errorf("a %d-byte response is over the %d-byte cap", length, maxResponseBytes)
	case length >= 0:
		body = make([]byte, length)
		_, err = io.ReadFull(r, body)
	default:
		body, err = readCapped(r)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("reading the body: %w", err)
	}
	return status, body, nil
}

// readChunked reads a chunked body to its last chunk and the trailers.
func readChunked(r *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(line, ";")
		n, err := strconv.ParseInt(strings.TrimSpace(size), 16, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("malformed chunk size %q", line)
		}
		if n == 0 {
			for {
				if t, err := readLine(r); err != nil || t == "" {
					return body, err
				}
			}
		}
		if int64(len(body))+n > maxResponseBytes {
			return nil, fmt.Errorf("the response is over the %d-byte cap", maxResponseBytes)
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		if crlf, err := readLine(r); err != nil || crlf != "" {
			return nil, errors.New("a chunk does not end in CRLF")
		}
	}
}

// readCapped reads to the server's close, up to maxResponseBytes.
func readCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err == nil && len(body) > maxResponseBytes {
		err = fmt.Errorf("the response is over the %d-byte cap", maxResponseBytes)
	}
	return body, err
}

// readLine reads one CRLF- (or LF-) terminated line, without the ending.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(line) > maxResponseBytes {
		return "", errors.New("a line is over the cap")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
