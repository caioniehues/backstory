package mcp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/ident"
	"github.com/RidgetopAi/backstory/internal/search"
)

// meaningFP is the fingerprint the fake embedders and the seeded vectors
// share.
const meaningFP = "sha256:test|tmpl1|vulkan|2"

// spyEmbedder is a search.Embedder that keeps every text it is asked to
// embed. Texts it has a vector for get it; any other text fails, so the
// Searcher falls back to text. block, when set, makes every call wait for
// it to close, ignoring ctx.
type spyEmbedder struct {
	vectors map[string][]float32
	err     error
	block   chan struct{}

	mu    sync.Mutex
	texts []string
}

func (s *spyEmbedder) Fingerprint(context.Context) (string, error) { return meaningFP, nil }

func (s *spyEmbedder) EmbedQuery(_ context.Context, text string) (string, []float32, error) {
	s.mu.Lock()
	s.texts = append(s.texts, text)
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	if s.err != nil {
		return "", nil, s.err
	}
	vec, ok := s.vectors[text]
	if !ok {
		return "", nil, errors.New("spy: no vector for this text")
	}
	return meaningFP, vec, nil
}

func (s *spyEmbedder) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// newMeaningEnv is newRelatedEnv with the daemon's embedders wired as the
// daemon wires them: query, the warm query server, backs recall's
// Searcher and embed_query; record, the batch path, backs note's
// related-Decisions check. A nil record means none is configured.
func newMeaningEnv(t *testing.T, query, record search.Embedder) *relatedEnv {
	t.Helper()
	st := mustOpenStore(t)
	selfPID := os.Getpid()
	procfs := fakeProcFS{
		status: map[int]ident.Status{selfPID: {PPid: 1, Name: "claude"}},
		cwd:    map[int]string{selfPID: "/home/brian/proj"},
	}
	resolver := &ident.Resolver{ProcFS: procfs, ProjectKey: func(string) string { return "proj-key" }}
	sessions := NewSessionRegistry()
	sock := filepath.Join(t.TempDir(), "sock")
	opts := []ServeOption{WithQueryEmbedder(query)}
	if record != nil {
		opts = append(opts, WithRecordEmbedder(record))
	}
	searcher := search.New(st, search.WithEmbedder(query))
	serveTestDaemon(t, sock, resolver, func(id ident.Identity, conn net.Conn) {
		ServeDaemonConn(id, conn, st, searcher, procfs, fakeGit{}, nil, sessions, captureNeverOff, nil, opts...)
	})
	return &relatedEnv{t: t, st: st, shim: dialShim(t, sock)}
}

func (e *relatedEnv) vector(id string, vec ...float32) {
	e.t.Helper()
	if ok, err := e.st.UpsertRecordEmbedding(id, meaningFP, vec); err != nil || !ok {
		e.t.Fatalf("UpsertRecordEmbedding(%s) = %v, %v", id, ok, err)
	}
}

// A Decision related only by meaning is found, and the new Decision's text
// is embedded by the record embedder (the batch path), never by the warm
// query server: the server holds query text only (user story 45).
func TestRelatedDecisionsByMeaningNeverReachTheQueryServer(t *testing.T) {
	const text = "marsupial pouch layout"
	query := &spyEmbedder{vectors: map[string][]float32{text: {1, 0}}}
	record := &spyEmbedder{vectors: map[string][]float32{text: {1, 0}}}
	e := newMeaningEnv(t, query, record)
	near := e.decision("use Postgres for the store")
	e.vector(near, 0.8, 0.6)
	far := e.decision("render the panel with QML")
	e.vector(far, 0, 1)

	r := e.note(map[string]any{"kind": "decision", "text": text})
	if ids := relatedIDs(r); !ids[near] || len(ids) != 1 {
		t.Fatalf("related_decisions = %+v, want only %s (found by meaning)", r.RelatedDecisions, near)
	}
	if got := record.seen(); len(got) == 0 || got[len(got)-1] != text {
		t.Errorf("record embedder saw %q, want the new Decision's text last", got)
	}
	if got := query.seen(); len(got) != 0 {
		t.Fatalf("the query server was sent Record text %q; note must embed through the batch path", got)
	}
}

// With no record embedder (embeddings off), or one that fails or misses
// the deadline, the check is text-only and the note still succeeds.
func TestRelatedDecisionsFallBackToTextWithoutTheRecordEmbedder(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	for name, record := range map[string]*spyEmbedder{
		"none":     nil,
		"off":      {err: search.ErrEmbeddingsOff},
		"deadline": {},
	} {
		t.Run(name, func(t *testing.T) {
			query := &spyEmbedder{}
			var rec search.Embedder
			if record != nil {
				rec = record
			}
			e := newMeaningEnv(t, query, rec)
			byText := e.decision("drop the quokka cache")
			if name == "deadline" {
				// Only the Decision under test waits: the seed's call has
				// returned.
				record.block = block
			}
			start := time.Now()
			r := e.note(map[string]any{"kind": "decision", "text": "keep the quokka cache"})
			if took := time.Since(start); took > RelatedEmbedDeadline+time.Second {
				t.Errorf("note took %v, want it bounded by RelatedEmbedDeadline (%v)", took, RelatedEmbedDeadline)
			}
			if ids := relatedIDs(r); !ids[byText] || len(ids) != 1 {
				t.Fatalf("related_decisions = %+v, want only %s (by text)", r.RelatedDecisions, byText)
			}
			if got := query.seen(); len(got) != 0 {
				t.Fatalf("the query server was sent Record text %q", got)
			}
		})
	}
}
