package search

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

type env struct {
	t        *testing.T
	st       *store.Store
	searcher Searcher
	sessions map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, st: st, searcher: New(st), sessions: map[string]string{}}
	for _, key := range []string{"proj-a", "proj-b"} {
		if err := st.UpsertProject(store.Project{Key: key, Toplevel: "/" + key, FirstSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		id, err := st.StartSession(store.StartSessionParams{
			Agent: "claude", CWD: "/" + key, ProjectKey: key, StartedAt: time.Now(), Origin: store.OriginLive,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.sessions[key] = id
	}
	return e
}

func (e *env) insert(project string, kind store.RecordKind, text, next string) string {
	e.t.Helper()
	id, err := e.st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: kind, Text: text, Next: next,
		SessionID: e.sessions[project], ProjectKey: project,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) search(q Query) Result {
	e.t.Helper()
	res, err := e.searcher.Search(context.Background(), q)
	if err != nil {
		e.t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func ids(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.RecordID
	}
	return out
}

func TestTextQueryHitsInRankOrderWithReason(t *testing.T) {
	e := newEnv(t)
	weak := e.insert("proj-a", store.KindNote, "a quokka among many other words in a long note", "")
	strong := e.insert("proj-a", store.KindNote, "quokka quokka", "")
	e.insert("proj-a", store.KindNote, "nothing relevant", "")

	res := e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 10})
	if got := ids(res.Hits); !slices.Equal(got, []string{strong, weak}) {
		t.Fatalf("hits = %v, want [%s %s]", got, strong, weak)
	}
	for _, h := range res.Hits {
		if h.Via != ViaText || h.Similarity != 0 {
			t.Errorf("hit %+v, want Via text and no similarity", h)
		}
	}
	if res.Reason != ReasonEmbeddingsOff {
		t.Errorf("Reason = %q, want %q", res.Reason, ReasonEmbeddingsOff)
	}
}

// Scope, kinds and Current only narrow the candidates before the limit, so
// better matches outside the filter never crowd out the one inside it.
func TestFiltersApplyBeforeRanking(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 10; i++ {
		e.insert("proj-b", store.KindDecision, "cache cache cache", "")
		e.insert("proj-a", store.KindNote, "cache cache cache", "")
	}
	want := e.insert("proj-a", store.KindDecision, "a decision touching the cache once among other words", "")
	old := e.insert("proj-a", store.KindDecision, "cache cache", "")
	repl := e.insert("proj-a", store.KindDecision, "replacement", "")
	if err := e.st.LinkEdge(repl, old, store.EdgeSupersedes, "human"); err != nil {
		t.Fatal(err)
	}
	ls, err := e.st.LocationScopeForKey("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	decisions := []store.RecordKind{store.KindDecision}
	for name, q := range map[string]Query{
		"project": {Text: "cache", ProjectKey: "proj-a", Kinds: decisions, CurrentOnly: true, Limit: 1},
		"scope":   {Text: "cache", Scope: &ls, Kinds: decisions, CurrentOnly: true, Limit: 1},
	} {
		if got := ids(e.search(q).Hits); !slices.Equal(got, []string{want}) {
			t.Errorf("%s: hits = %v, want [%s]", name, got, want)
		}
	}
}

func TestTombstonedRecordsAreNeverHits(t *testing.T) {
	e := newEnv(t)
	gone := e.insert("proj-a", store.KindDecision, "zebrafinch layout", "")
	kept := e.insert("proj-a", store.KindDecision, "zebrafinch rewrite", "")
	if err := e.st.TombstoneRecord(gone, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	if got := ids(e.search(Query{Text: "zebrafinch", ProjectKey: "proj-a", Limit: 10}).Hits); !slices.Equal(got, []string{kept}) {
		t.Fatalf("hits = %v, want [%s]", got, kept)
	}
}

func TestHandoffFoundByNext(t *testing.T) {
	e := newEnv(t)
	id := e.insert("proj-a", store.KindHandoff, "wired the parser", "NEXTWORD finish the migration")
	e.insert("proj-a", store.KindHandoff, "another handoff", "something else")
	if got := ids(e.search(Query{Text: "nextword", ProjectKey: "proj-a", Limit: 10}).Hits); !slices.Equal(got, []string{id}) {
		t.Fatalf("hits = %v, want [%s]", got, id)
	}
}

// A Record query needs the vector side, which is off: no hits, and the
// reason says why rather than an error.
func TestRecordQueryWithEmbeddingsOff(t *testing.T) {
	e := newEnv(t)
	id := e.insert("proj-a", store.KindNote, "anything", "")
	res := e.search(Query{RecordID: id, ProjectKey: "proj-a", Limit: 10})
	if len(res.Hits) != 0 || res.Reason != ReasonEmbeddingsOff {
		t.Fatalf("result = %+v, want no hits and reason %q", res, ReasonEmbeddingsOff)
	}
}

func TestQueryIsTextOrRecordNotBoth(t *testing.T) {
	e := newEnv(t)
	if _, err := e.searcher.Search(context.Background(), Query{Text: "x", RecordID: "y", Limit: 1}); err == nil {
		t.Fatal("want an error for a query with both text and a Record id")
	}
}

func TestKeyTermsOnlyReachesTheTextSide(t *testing.T) {
	e := newEnv(t)
	sqlite := e.insert("proj-a", store.KindDecision, "keep SQLite as the store", "")
	e.insert("proj-a", store.KindDecision, "use Postgres for reports", "")
	got := ids(e.search(Query{Text: "Use SQLite", ProjectKey: "proj-a", KeyTermsOnly: true, Limit: 10}).Hits)
	if !slices.Equal(got, []string{sqlite}) {
		t.Fatalf("hits = %v, want only [%s]", got, sqlite)
	}
}
