package mcp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/ident"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// fakeQueryEmbedder stands in for the daemon's warm query server.
type fakeQueryEmbedder struct {
	err   error
	block chan struct{} // when set, calls wait on it, ignoring ctx

	mu    sync.Mutex
	texts []string
}

func (f *fakeQueryEmbedder) Fingerprint(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "fp-test", nil
}

func (f *fakeQueryEmbedder) EmbedQuery(_ context.Context, text string) (string, []float32, error) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return "", nil, f.err
	}
	return "fp-test", []float32{float32(len(text)), 0.5, -0.25}, nil
}

// embedQueryDaemon serves ServeDaemonConn on a socket for a known harness,
// with emb as the daemon's query embedder (none when nil).
func embedQueryDaemon(t *testing.T, st *store.Store, emb search.Embedder) string {
	t.Helper()
	selfPID := os.Getpid()
	procfs := fakeProcFS{
		status: map[int]ident.Status{selfPID: {PPid: 1, Name: "claude"}},
		cwd:    map[int]string{selfPID: "/home/brian/proj"},
	}
	resolver := &ident.Resolver{ProcFS: procfs, ProjectKey: func(string) string { return "proj-key" }}
	sessions := NewSessionRegistry()
	sockPath := filepath.Join(t.TempDir(), "sock")
	var opts []ServeOption
	if emb != nil {
		opts = append(opts, WithQueryEmbedder(emb))
	}
	serveTestDaemon(t, sockPath, resolver, func(id ident.Identity, conn net.Conn) {
		ServeDaemonConn(id, conn, st, search.New(st), procfs, fakeGit{}, nil, sessions, captureNeverOff, nil, opts...)
	})
	return sockPath
}

func sessionCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The CLI's embedder gets the daemon's vector and fingerprint over the
// socket, and asking mints no session: a CLI recall isn't agent work.
func TestEmbedQueryAnswersOverTheDaemonSocket(t *testing.T) {
	st := mustOpenStore(t)
	emb := &fakeQueryEmbedder{}
	client := DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, emb)}

	fp, vec, err := client.EmbedQuery(context.Background(), "why did we drop the cache")
	if err != nil {
		t.Fatal(err)
	}
	if fp != "fp-test" || len(vec) != 3 || vec[0] != 25 || vec[1] != 0.5 || vec[2] != -0.25 {
		t.Errorf("got %q %v, want fp-test [25 0.5 -0.25]", fp, vec)
	}
	if len(emb.texts) != 1 || emb.texts[0] != "why did we drop the cache" {
		t.Errorf("the daemon embedded %q, want the query text as given", emb.texts)
	}
	if got, err := client.Fingerprint(context.Background()); err != nil || got != "fp-test" {
		t.Errorf("Fingerprint = %q, %v; want fp-test", got, err)
	}
	if len(emb.texts) != 1 {
		t.Errorf("Fingerprint embedded text: %q", emb.texts)
	}
	if n := sessionCount(t, st); n != 0 {
		t.Errorf("%d sessions after embed queries, want none", n)
	}
}

// The daemon enforces the deadline itself, below the Searcher's, so a
// slow query server reads as a missed deadline, not a hang.
func TestEmbedQueryEnforcesTheDeadline(t *testing.T) {
	if EmbedQueryDeadline >= search.QueryEmbedDeadline {
		t.Fatalf("EmbedQueryDeadline = %v, want it under the Searcher's %v", EmbedQueryDeadline, search.QueryEmbedDeadline)
	}
	st := mustOpenStore(t)
	emb := &fakeQueryEmbedder{block: make(chan struct{})}
	t.Cleanup(func() { close(emb.block) })
	client := DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, emb)}

	start := time.Now()
	_, _, err := client.EmbedQuery(context.Background(), "slow")
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a missed deadline", err)
	}
	if took < EmbedQueryDeadline || took > time.Second {
		t.Errorf("answered after %v, want about the daemon's %v", took, EmbedQueryDeadline)
	}

	res, err := search.New(st, search.WithEmbedder(client)).Search(context.Background(), search.Query{Text: "slow", ProjectKey: "p", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if want := "embeddings unavailable: query embedding missed the 100ms deadline"; res.Reason != want {
		t.Errorf("Reason = %q, want %q", res.Reason, want)
	}
}

// Every fallback the hybrid-search ticket names, as the CLI's Searcher
// reads it through the socket.
func TestEmbedQueryFallbackReasons(t *testing.T) {
	st := mustOpenStore(t)
	for _, c := range []struct {
		name   string
		client DaemonEmbedder
		want   string
	}{
		{"no query embedder", DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, nil)}, "embeddings off"},
		{"embeddings switched off", DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, &fakeQueryEmbedder{err: search.ErrEmbeddingsOff})}, "embeddings off"},
		{"the runtime fails", DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, &fakeQueryEmbedder{err: errors.New("the query server exited (exit status 1); restarting in 2s")})},
			"embeddings unavailable: the query server exited (exit status 1); restarting in 2s"},
		{"no daemon", DaemonEmbedder{SocketPath: filepath.Join(t.TempDir(), "sock")}, "embeddings unavailable: daemon unreachable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := search.New(st, search.WithEmbedder(c.client)).Search(context.Background(), search.Query{Text: "anything", ProjectKey: "p", Limit: 5})
			if err != nil {
				t.Fatal(err)
			}
			if res.Reason != c.want {
				t.Errorf("Reason = %q, want %q", res.Reason, c.want)
			}
			if res, _ := search.New(st, search.WithEmbedder(c.client)).Search(context.Background(), search.Query{RecordID: "r1", ProjectKey: "p", Limit: 5}); res.Reason != c.want {
				t.Errorf("Record query Reason = %q, want %q", res.Reason, c.want)
			}
		})
	}
}

// A request with no text is refused, not embedded.
func TestEmbedQueryRefusesEmptyText(t *testing.T) {
	st := mustOpenStore(t)
	emb := &fakeQueryEmbedder{}
	client := DaemonEmbedder{SocketPath: embedQueryDaemon(t, st, emb)}
	if _, _, err := client.EmbedQuery(context.Background(), "  "); err == nil || !strings.Contains(err.Error(), "text") {
		t.Errorf("err = %v, want the missing text named", err)
	}
	if len(emb.texts) != 0 {
		t.Errorf("embedded %q", emb.texts)
	}
}
