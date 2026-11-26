package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/RidgetopAi/backstory/internal/search"
)

// ServerBinary is llama-server's file name. The query server runs the
// llama-server beside the stored runtime (llama-embedding): the same build,
// so the same device list the stored match is resolved against (#41's
// notes: resolve with llama-embedding, pass the device to llama-server).
const ServerBinary = "llama-server"

// Query server tunables. Named here, never inlined.
const (
	// SetupPoll is how often the query server re-reads the setup, so that
	// `embed --on` starts the server and `--off` stops it.
	SetupPoll = 15 * time.Second
	// ServerStartTimeout bounds a start, from exec to /health answering OK.
	// #19 measured ready in about 0.33 s; a first run on a cold shader cache
	// takes longer.
	ServerStartTimeout = time.Minute
	// StableRun is how long a server must have run for its exit to count as
	// a fresh failure, starting the backoff over.
	StableRun = time.Minute
	// serverStopGrace is how long a stopped server gets to exit on SIGTERM
	// before it is killed.
	serverStopGrace = 5 * time.Second
	// readyPoll is how often a starting server's /health is asked.
	readyPoll = 20 * time.Millisecond
)

// errStarting is the query server's answer while its llama-server starts.
var errStarting = errors.New("the query server is starting")

// QueryServerConfig configures StartQueryServer. Zero durations and a nil
// Backoff take the named defaults.
type QueryServerConfig struct {
	// Settings holds the setup (LoadSetup) and the model hash cache.
	Settings Settings
	// Socket is the unix socket llama-server listens on. It must end in
	// ".sock" and sit in a directory only this user can enter: the socket
	// exists for a moment before it is made 0600.
	Socket string
	// Poll replaces SetupPoll, StartTimeout ServerStartTimeout and
	// StableRun StableRun.
	Poll, StartTimeout, StableRun time.Duration
	// Backoff paces restarts after a crash or a failed start.
	Backoff *Backoff
	// Logf, when set, gets a line for each start, failure and stop. No line
	// carries query text.
	Logf func(format string, args ...any)
}

// QueryServer is the query embedder: a warm llama-server the daemon starts,
// supervises and stops. It follows the setup, starting the server while
// embeddings are on and stopping it when they go off or the setup changes,
// and restarts a crashed server after a backoff. Only query text goes
// through it, with QueryPrefix; Record text never does, so the server holds
// no Record text and needs no restart after a Delete or Purge. The server
// keeps no prompt cache (--cache-ram 0), saves no slots, exposes no slots
// endpoint and writes no logs.
//
// It is a search.Embedder: the daemon's own Searcher calls it in-process.
type QueryServer struct {
	cfg    QueryServerConfig
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	state queryState
}

// queryState is what EmbedQuery sees.
type queryState struct {
	on    bool
	ready bool
	fp    string
	err   error // why it isn't ready
}

var _ search.Embedder = (*QueryServer)(nil)

// StartQueryServer starts supervising. The first setup read happens before
// it returns, so a query right after already sees embeddings on or off.
func StartQueryServer(cfg QueryServerConfig) *QueryServer {
	if cfg.Poll <= 0 {
		cfg.Poll = SetupPoll
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = ServerStartTimeout
	}
	if cfg.StableRun <= 0 {
		cfg.StableRun = StableRun
	}
	if cfg.Backoff == nil {
		cfg.Backoff = &Backoff{}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &QueryServer{cfg: cfg, cancel: cancel, done: make(chan struct{})}
	setup, on, err := LoadSetup(cfg.Settings)
	q.state = queryState{on: on, err: errStarting}
	if err != nil {
		q.state.err = fmt.Errorf("reading the setup: %w", err)
	}
	go q.run(ctx, setup, on, err)
	return q
}

// Close stops supervising and stops the server, waiting for it to exit.
func (q *QueryServer) Close() {
	q.cancel()
	<-q.done
}

func (q *QueryServer) snapshot() queryState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

func (q *QueryServer) set(s queryState) {
	q.mu.Lock()
	q.state = s
	q.mu.Unlock()
}

// Fingerprint is the fingerprint the running server's vectors are made
// under: the same one the drain writes for this setup.
func (q *QueryServer) Fingerprint(context.Context) (string, error) {
	s := q.snapshot()
	switch {
	case !s.on:
		return "", search.ErrEmbeddingsOff
	case !s.ready:
		return "", s.err
	}
	return s.fp, nil
}

// EmbedQuery embeds text, with QueryPrefix, on the warm server. It answers
// search.ErrEmbeddingsOff while embeddings are off, and why the server
// isn't ready while it starts or after it failed. It honours ctx: the
// deadline is the caller's.
func (q *QueryServer) EmbedQuery(ctx context.Context, text string) (string, []float32, error) {
	s := q.snapshot()
	switch {
	case !s.on:
		return "", nil, search.ErrEmbeddingsOff
	case !s.ready:
		return "", nil, s.err
	}
	body, err := json.Marshal(struct {
		Input string `json:"input"`
	}{QueryPrefix + text})
	if err != nil {
		return "", nil, err
	}
	status, resp, err := unixHTTP(ctx, q.cfg.Socket, "POST", "/v1/embeddings", body)
	if err != nil {
		return "", nil, err
	}
	if status != 200 {
		return "", nil, fmt.Errorf("query server: %s", serverError(status, resp))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", nil, fmt.Errorf("query server: reading the answer: %w", err)
	}
	if len(out.Data) != 1 {
		return "", nil, fmt.Errorf("query server: %d vectors for one query", len(out.Data))
	}
	if n := len(out.Data[0].Embedding); n != Dim {
		return "", nil, fmt.Errorf("query server: a %d-dimensional vector, want %d", n, Dim)
	}
	return s.fp, out.Data[0].Embedding, nil
}

// serverError is a non-200 answer's message: llama-server's
// {"error":{"message":...}}, or the bare status.
func serverError(status int, body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", status, e.Error.Message)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// run is the supervisor: it follows the setup, starts the server while
// embeddings are on, and restarts it after a crash once the backoff allows.
func (q *QueryServer) run(ctx context.Context, setup Setup, on bool, loadErr error) {
	defer close(q.done)
	var (
		cur      *serverProc
		curSetup Setup
		retryAt  time.Time
	)
	stopCur := func(why string) {
		if cur == nil {
			return
		}
		cur.stop()
		q.cfg.Logf("embed: query server stopped (%s)", why)
		cur = nil
		_ = os.Remove(q.cfg.Socket)
	}
	poll := time.NewTicker(q.cfg.Poll)
	defer poll.Stop()
	for {
		switch {
		case loadErr != nil:
			// Keep what runs; say why nothing new starts.
			if cur == nil {
				q.set(queryState{on: true, err: fmt.Errorf("reading the setup: %w", loadErr)})
			}
		case !on:
			stopCur("embeddings off")
			q.cfg.Backoff.Reset()
			retryAt = time.Time{}
			q.set(queryState{})
		case cur != nil && setup != curSetup:
			stopCur("the setup changed")
			q.cfg.Backoff.Reset()
			retryAt = time.Time{}
		}
		if loadErr == nil && on && cur == nil && !time.Now().Before(retryAt) {
			q.set(queryState{on: true, err: errStarting})
			p, fp, err := q.start(ctx, setup)
			switch {
			case ctx.Err() != nil:
				stopCur("the daemon is stopping")
				return
			case err != nil:
				delay := q.cfg.Backoff.Next()
				retryAt = time.Now().Add(delay)
				q.cfg.Logf("embed: query server failed to start, retrying in %s: %v", delay, err)
				q.set(queryState{on: true, err: err})
			default:
				cur, curSetup = p, setup
				q.cfg.Logf("embed: query server ready on %s", q.cfg.Socket)
				q.set(queryState{on: true, ready: true, fp: fp})
			}
		}

		var exited <-chan struct{}
		if cur != nil {
			exited = cur.exited
		}
		var retry <-chan time.Time
		var timer *time.Timer
		if on && cur == nil && !retryAt.IsZero() {
			timer = time.NewTimer(time.Until(retryAt))
			retry = timer.C
		}
		select {
		case <-ctx.Done():
			stopCur("the daemon is stopping")
			q.set(queryState{})
			return
		case <-poll.C:
		case <-retry:
		case <-exited:
			if time.Since(cur.started) >= q.cfg.StableRun {
				q.cfg.Backoff.Reset()
			}
			delay := q.cfg.Backoff.Next()
			retryAt = time.Now().Add(delay)
			err := fmt.Errorf("the query server exited (%v); restarting in %s", cur.err, delay)
			q.cfg.Logf("embed: %v", err)
			cur = nil
			_ = os.Remove(q.cfg.Socket)
			q.set(queryState{on: true, err: err})
		}
		if timer != nil {
			timer.Stop()
		}
		setup, on, loadErr = LoadSetup(q.cfg.Settings)
	}
}

// serverProc is one running llama-server.
type serverProc struct {
	cmd     *exec.Cmd
	started time.Time
	exited  chan struct{}
	err     error // the exit, once exited is closed
}

// stop asks the server to exit and kills it if it doesn't in time.
func (p *serverProc) stop() {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(serverStopGrace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// serverArgs is llama-server's command line: the resolved device, the
// private socket, one slot, no prompt cache, no slots endpoint, no logs and
// no network. There is never a --slot-save-path.
func serverArgs(s Setup, dev Device, sock string) []string {
	args := []string{"-m", s.Model, "-dev", dev.Arg}
	if dev.Backend != cpuDevice.Backend {
		args = append(args, "-ngl", "all")
	}
	return append(args, "-fit", "off", "--embedding", "--host", sock, "--no-webui", "--no-slots",
		"-np", "1", "--cache-ram", "0", "--log-disable", "--offline")
}

// start checks the files, resolves the device, starts llama-server on it
// and waits until it answers /health. It returns the fingerprint its
// vectors are made under.
func (q *QueryServer) start(ctx context.Context, s Setup) (*serverProc, string, error) {
	if err := CheckFiles(s); err != nil {
		return nil, "", err
	}
	server := filepath.Join(filepath.Dir(s.Runtime), ServerBinary)
	if err := CheckFiles(Setup{Runtime: server, Model: s.Model}); err != nil {
		return nil, "", err
	}
	dev, err := ResolveDevice(ctx, s)
	if err != nil {
		return nil, "", err
	}
	hash, err := ModelHash(q.cfg.Settings, s.Model)
	if err != nil {
		return nil, "", err
	}
	fp := Fingerprint(hash, dev.Backend, Dim)

	if err := os.MkdirAll(filepath.Dir(q.cfg.Socket), 0o700); err != nil {
		return nil, "", fmt.Errorf("embed: query server socket dir: %w", err)
	}
	if err := os.Remove(q.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("embed: removing a stale query server socket: %w", err)
	}
	cmd := exec.Command(server, serverArgs(s, dev, q.cfg.Socket)...) //nolint:gosec // the llama-server beside the Human's chosen runtime; no query text in args
	// No stdin, and stdout/stderr go to /dev/null: the server keeps no log.
	// It dies with the daemon even if the daemon is killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("embed: starting %s: %w", server, err)
	}
	p := &serverProc{cmd: cmd, started: time.Now(), exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	if err := q.waitReady(ctx, p); err != nil {
		p.stop()
		_ = os.Remove(q.cfg.Socket)
		return nil, "", err
	}
	return p, fp, nil
}

// waitReady makes the socket 0600 as soon as it appears and waits for
// /health to answer 200.
func (q *QueryServer) waitReady(ctx context.Context, p *serverProc) error {
	deadline := time.Now().Add(q.cfg.StartTimeout)
	private := false
	last := errStarting
	for {
		if !private {
			if err := os.Chmod(q.cfg.Socket, 0o600); err == nil {
				private = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("embed: making the query server socket private: %w", err)
			}
		}
		if private {
			hctx, cancel := context.WithTimeout(ctx, time.Second)
			status, body, err := unixHTTP(hctx, q.cfg.Socket, "GET", "/health", nil)
			cancel()
			switch {
			case err != nil:
				last = err
			case status == 200:
				return nil
			default:
				last = fmt.Errorf("query server: %s", serverError(status, body))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.exited:
			return fmt.Errorf("embed: llama-server exited while starting: %v", p.err)
		case <-time.After(readyPoll):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embed: llama-server not ready after %s: %w", q.cfg.StartTimeout, last)
		}
	}
}
