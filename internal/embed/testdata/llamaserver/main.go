// Command llamaserver stands in for llama.cpp's llama-server (b12359) in
// internal/embed's and cmd/backstory's tests: no GPU, no model. Tests build
// it to a file named llama-server beside the llama-embedding stub, where the
// query server looks for it. It listens on the unix socket --host names and
// answers GET /health and POST /v1/embeddings in the real server's shapes.
// Each vector is [input bytes, 1, 0, ...], so a test can tell what was sent.
//
// It uses net/http on purpose: it is test code under testdata, outside the
// shipped tree TestNoNetworkInShippedCode scans, and a real HTTP server
// checks the hand-rolled client better than a second hand-rolled one would
// (net/http sends a vector this long chunked, /health with a length).
//
//	STUB_SERVER_ARGV    file the arguments are written to, one per line
//	STUB_SERVER_STARTS  file that gets this process's pid, one line per start
//	STUB_SERVER_INPUTS  file each embedding input is appended to, quoted
//	STUB_SERVER_DELAY   duration each embedding waits before answering
//	STUB_SERVER_SLOW    set: only inputs containing it wait STUB_SERVER_DELAY
//	STUB_SERVER_LOAD    duration /health answers 503 "Loading model"
//	STUB_SERVER_EXIT_AFTER  duration after which the server exits 1
//	STUB_SERVER_CRASH   file: when it exists, the next embedding request
//	                    removes it and the server exits 1 (a crash)
//	STUB_SERVER_FAIL    set: exit 1 at start (a runtime that can't load)
//	STUB_DIM            vector length (default 768)
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if p := os.Getenv("STUB_SERVER_ARGV"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o600)
	}
	if p := os.Getenv("STUB_SERVER_STARTS"); p != "" {
		appendLine(p, strconv.Itoa(os.Getpid()))
	}
	if os.Getenv("STUB_SERVER_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "stub llama-server: failed to load model")
		os.Exit(1)
	}
	host := ""
	for i, a := range os.Args {
		if a == "--host" && i+1 < len(os.Args) {
			host = os.Args[i+1]
		}
	}
	if !strings.HasSuffix(host, ".sock") {
		fmt.Fprintln(os.Stderr, "stub llama-server: --host must be a unix socket path ending in .sock")
		os.Exit(1)
	}
	ln, err := net.Listen("unix", host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub llama-server:", err)
		os.Exit(1)
	}
	loaded := time.Now().Add(duration("STUB_SERVER_LOAD"))
	if d := duration("STUB_SERVER_EXIT_AFTER"); d > 0 {
		time.AfterFunc(d, func() { os.Exit(1) })
	}
	dim := 768
	if v, err := strconv.Atoi(os.Getenv("STUB_DIM")); err == nil {
		dim = v
	}

	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if time.Now().Before(loaded) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(loaded) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`))
			return
		}
		var req struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad json","type":"invalid_request_error"}}`))
			return
		}
		if p := os.Getenv("STUB_SERVER_CRASH"); p != "" {
			if os.Remove(p) == nil {
				os.Exit(1)
			}
		}
		mu.Lock()
		if p := os.Getenv("STUB_SERVER_INPUTS"); p != "" {
			appendLine(p, strconv.Quote(req.Input))
		}
		mu.Unlock()
		if slow := os.Getenv("STUB_SERVER_SLOW"); slow == "" || strings.Contains(req.Input, slow) {
			time.Sleep(duration("STUB_SERVER_DELAY"))
		}
		// The filler makes the answer as long as a real one (about 9 KB),
		// so net/http sends it chunked.
		vec := make([]float32, dim)
		for i := range vec {
			vec[i] = 0.012345678
		}
		vec[0], vec[1] = float32(len(req.Input)), 1
		out := map[string]any{
			"model": "stub", "object": "list",
			"data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": vec}},
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	_ = http.Serve(ln, mux) //nolint:gosec // a test stub on a unix socket in the test's temp dir
}

func duration(env string) time.Duration {
	d, _ := time.ParseDuration(os.Getenv(env))
	return d
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the test's own temp file
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}
