package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

const (
	testFingerprint = "sha256:test|tmpl1|vulkan|2"
	oldFingerprint  = "sha256:old|tmpl1|vulkan|2"
)

// fakeEmbedder embeds query text by lookup. err fails every call; block,
// when set, makes every call wait for it to close, ignoring ctx, so only the
// Searcher's own deadline can cut it short.
type fakeEmbedder struct {
	vectors map[string][]float32
	err     error
	block   chan struct{}
	embeds  atomic.Int32
}

func (f *fakeEmbedder) wait() error {
	if f.block != nil {
		<-f.block
	}
	return f.err
}

func (f *fakeEmbedder) Fingerprint(context.Context) (string, error) {
	if err := f.wait(); err != nil {
		return "", err
	}
	return testFingerprint, nil
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, text string) (string, []float32, error) {
	f.embeds.Add(1)
	if err := f.wait(); err != nil {
		return "", nil, err
	}
	vec, ok := f.vectors[text]
	if !ok {
		return "", nil, errors.New("fake: no vector for " + text)
	}
	return testFingerprint, vec, nil
}

// newHybridEnv is newEnv with a Searcher whose vector side uses emb, with
// the options given (the cosine floor 0.5 unless they set another).
func newHybridEnv(t *testing.T, emb Embedder, opts ...Option) *env {
	t.Helper()
	e := newEnv(t)
	e.searcher = New(e.st, append([]Option{WithEmbedder(emb), WithCosineFloor(0.5)}, opts...)...)
	return e
}

func (e *env) vector(id, fingerprint string, vec ...float32) {
	e.t.Helper()
	if ok, err := e.st.UpsertRecordEmbedding(id, fingerprint, vec); err != nil || !ok {
		e.t.Fatalf("UpsertRecordEmbedding(%s) = %v, %v", id, ok, err)
	}
}

// insertVec inserts a proj-a Note with a vector under the test fingerprint.
func (e *env) insertVec(text string, vec ...float32) string {
	e.t.Helper()
	id := e.insert("proj-a", store.KindNote, text, "")
	e.vector(id, testFingerprint, vec...)
	return id
}

var east = []float32{1, 0}

func queryEast(texts ...string) *fakeEmbedder {
	f := &fakeEmbedder{vectors: map[string][]float32{}}
	for _, t := range texts {
		f.vectors[t] = east
	}
	return f
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Each side ranks its own candidates and RRF (k = 60) fuses them: a Record
// both sides found outranks each side's best, and on a tie the text side's
// Record comes first.
func TestTextQueryFusesBothSidesByRRF(t *testing.T) {
	e := newHybridEnv(t, queryEast("quokka"))
	t1 := e.insertVec("quokka quokka quokka", 0, 1)
	t2 := e.insertVec("quokka quokka and a few more words", 0, 1)
	both := e.insertVec("one quokka among very many other unrelated words in this note", 0.6, 0.8)
	v1 := e.insertVec("marsupial", 1, 0)
	v2 := e.insertVec("small wallaby", 0.8, 0.6)
	e.insertVec("unrelated", 0, 1)

	res := e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 10})
	if got, want := ids(res.Hits), []string{both, t1, v1, t2, v2}; !slices.Equal(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
	want := map[string]struct {
		via Via
		sim float64
	}{both: {ViaBoth, 0.6}, t1: {ViaText, 0}, t2: {ViaText, 0}, v1: {ViaVector, 1}, v2: {ViaVector, 0.8}}
	for _, h := range res.Hits {
		if w := want[h.RecordID]; h.Via != w.via || !near(h.Similarity, w.sim) {
			t.Errorf("hit %s = %s %v, want %s %v", h.RecordID, h.Via, h.Similarity, w.via, w.sim)
		}
	}
	if res.Reason != "" {
		t.Errorf("Reason = %q, want none (both sides contributed)", res.Reason)
	}

	if got := ids(e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 2}).Hits); !slices.Equal(got, []string{both, t1}) {
		t.Fatalf("Limit 2: hits = %v, want [%s %s]", got, both, t1)
	}
}

// An exact token the model knows nothing about still ranks: the text side's
// top hit is not drowned by the vector side's.
func TestExactTokenMatchStillRanks(t *testing.T) {
	e := newHybridEnv(t, queryEast("wobble-party", "9f3c2ab"))
	party := e.insertVec("the wobble-party flag stays", 0, 1)
	hash := e.insertVec("reverted 9f3c2ab after the outage", 0, 1)
	for i := 0; i < 5; i++ {
		e.insertVec("something the model thinks is close", 1, 0)
	}
	for token, want := range map[string]string{"wobble-party": party, "9f3c2ab": hash} {
		hits := e.search(Query{Text: token, ProjectKey: "proj-a", Limit: 3}).Hits
		if len(hits) == 0 || hits[0].RecordID != want || hits[0].Via != ViaText {
			t.Errorf("%s: hits = %+v, want %s first, via text", token, hits, want)
		}
	}
}

func TestCosineFloorDropsWeakVectorHits(t *testing.T) {
	emb := queryEast("query")
	e := newHybridEnv(t, emb, WithCosineFloor(0.7))
	strong := e.insertVec("close", 0.8, 0.6)
	weak := e.insertVec("further", 0.6, 0.8)
	if got := ids(e.search(Query{Text: "query", ProjectKey: "proj-a", Limit: 10}).Hits); !slices.Equal(got, []string{strong}) {
		t.Fatalf("floor 0.7: hits = %v, want [%s]", got, strong)
	}
	e.searcher = New(e.st, WithEmbedder(emb), WithCosineFloor(0.5))
	if got := ids(e.search(Query{Text: "query", ProjectKey: "proj-a", Limit: 10}).Hits); !slices.Equal(got, []string{strong, weak}) {
		t.Fatalf("floor 0.5: hits = %v, want [%s %s]", got, strong, weak)
	}
}

// The vector side gets the same filters, before ranking, and never returns
// a Tombstoned Record.
func TestVectorSideFiltersBeforeRanking(t *testing.T) {
	e := newHybridEnv(t, queryEast("query"))
	for i := 0; i < 5; i++ {
		e.vector(e.insert("proj-b", store.KindDecision, "other project", ""), testFingerprint, 1, 0)
		e.vector(e.insert("proj-a", store.KindNote, "other kind", ""), testFingerprint, 1, 0)
	}
	want := e.insert("proj-a", store.KindDecision, "inside", "")
	e.vector(want, testFingerprint, 0.6, 0.8)
	old := e.insert("proj-a", store.KindDecision, "superseded", "")
	e.vector(old, testFingerprint, 1, 0)
	repl := e.insert("proj-a", store.KindDecision, "replacement", "")
	e.vector(repl, testFingerprint, 0, 1)
	if err := e.st.LinkEdge(repl, old, store.EdgeSupersedes, "human"); err != nil {
		t.Fatal(err)
	}
	gone := e.insert("proj-a", store.KindDecision, "forgotten", "")
	e.vector(gone, testFingerprint, 1, 0)
	if err := e.st.TombstoneRecord(gone, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	ls, err := e.st.LocationScopeForKey("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	decisions := []store.RecordKind{store.KindDecision}
	for name, q := range map[string]Query{
		"project": {Text: "query", ProjectKey: "proj-a", Kinds: decisions, CurrentOnly: true, Limit: 1},
		"scope":   {Text: "query", Scope: &ls, Kinds: decisions, CurrentOnly: true, Limit: 1},
	} {
		if got := ids(e.search(q).Hits); !slices.Equal(got, []string{want}) {
			t.Errorf("%s: hits = %v, want [%s]", name, got, want)
		}
	}
}

// While some Records have no vector for the current fingerprint (a model
// changeover, or not embedded yet), the result is partial and says so.
// Vectors of the old model are never compared with the query.
func TestPartialWhileRecordsLackACurrentVector(t *testing.T) {
	e := newHybridEnv(t, queryEast("marsupial"))
	current := e.insertVec("wallaby", 1, 0)
	stale := e.insert("proj-a", store.KindNote, "kangaroo", "")
	e.vector(stale, oldFingerprint, 1, 0)
	text := e.insert("proj-a", store.KindNote, "marsupial words", "")

	res := e.search(Query{Text: "marsupial", ProjectKey: "proj-a", Limit: 10})
	if got := ids(res.Hits); !slices.Equal(got, []string{text, current}) {
		t.Fatalf("hits = %v, want [%s %s] (the stale vector is ignored)", got, text, current)
	}
	if want := "embeddings partial: 2 Records not embedded yet"; res.Reason != want {
		t.Errorf("Reason = %q, want %q", res.Reason, want)
	}
}

// A text query falls back to FTS-only when the Embedder fails, and says
// why; it never errors because of the vector side.
func TestTextQueryFallsBackWhenTheEmbedderFails(t *testing.T) {
	e := newHybridEnv(t, &fakeEmbedder{err: errors.New("daemon unreachable")})
	hit := e.insertVec("quokka", 1, 0)
	res := e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 10})
	if got := ids(res.Hits); !slices.Equal(got, []string{hit}) || res.Hits[0].Via != ViaText {
		t.Fatalf("hits = %+v, want [%s] via text", res.Hits, hit)
	}
	if want := "embeddings unavailable: daemon unreachable"; res.Reason != want {
		t.Errorf("Reason = %q, want %q", res.Reason, want)
	}
}

// An Embedder that doesn't answer in time is abandoned at the deadline, even
// one that ignores its context.
func TestTextQueryFallsBackWhenTheEmbedderMissesTheDeadline(t *testing.T) {
	emb := &fakeEmbedder{block: make(chan struct{})}
	t.Cleanup(func() { close(emb.block) })
	e := newHybridEnv(t, emb, WithDeadline(20*time.Millisecond))
	hit := e.insertVec("quokka", 1, 0)

	start := time.Now()
	res := e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 10})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Search took %v, want it cut short at the deadline", took)
	}
	if got := ids(res.Hits); !slices.Equal(got, []string{hit}) {
		t.Fatalf("hits = %v, want [%s]", got, hit)
	}
	if want := "embeddings unavailable: query embedding missed the 20ms deadline"; res.Reason != want {
		t.Errorf("Reason = %q, want %q", res.Reason, want)
	}
}

func TestQueryEmbedDeadlineIs100ms(t *testing.T) {
	emb := &fakeEmbedder{block: make(chan struct{})}
	t.Cleanup(func() { close(emb.block) })
	e := newHybridEnv(t, emb)
	start := time.Now()
	res := e.search(Query{Text: "anything", ProjectKey: "proj-a", Limit: 10})
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("gave up after %v, want the 100ms deadline", took)
	}
	if !strings.HasSuffix(res.Reason, "missed the 100ms deadline") {
		t.Errorf("Reason = %q, want the 100ms deadline named", res.Reason)
	}
}

// A Record query finds the Record's Neighbours from its stored vector:
// nothing is embedded, and the Record itself is not its own Neighbour.
func TestRecordQueryFindsNeighboursFromTheStoredVector(t *testing.T) {
	emb := queryEast()
	e := newHybridEnv(t, emb)
	anchor := e.insertVec("the anchor", 1, 0)
	closer := e.insertVec("closer", 0.8, 0.6)
	further := e.insertVec("further", 0.6, 0.8)
	e.insertVec("too far", 0, 1)

	res := e.search(Query{RecordID: anchor, ProjectKey: "proj-a", Limit: 10})
	if got := ids(res.Hits); !slices.Equal(got, []string{closer, further}) {
		t.Fatalf("hits = %v, want [%s %s]", got, closer, further)
	}
	for _, h := range res.Hits {
		if h.Via != ViaVector || h.Similarity < 0.5 {
			t.Errorf("hit %+v, want via vector with its cosine", h)
		}
	}
	if !near(res.Hits[0].Similarity, 0.8) {
		t.Errorf("similarity = %v, want 0.8", res.Hits[0].Similarity)
	}
	if res.Reason != "" {
		t.Errorf("Reason = %q, want none", res.Reason)
	}
	if n := emb.embeds.Load(); n != 0 {
		t.Errorf("EmbedQuery called %d times, want 0 (a Record query uses the stored vector)", n)
	}
	if got := ids(e.search(Query{RecordID: anchor, ProjectKey: "proj-a", Limit: 1}).Hits); !slices.Equal(got, []string{closer}) {
		t.Errorf("Limit 1: hits = %v, want [%s]", got, closer)
	}
}

func TestRecordQueryWithoutACurrentVectorIsEmptyWithAReason(t *testing.T) {
	e := newHybridEnv(t, queryEast())
	anchor := e.insert("proj-a", store.KindNote, "embedded by the old model", "")
	e.vector(anchor, oldFingerprint, 1, 0)
	e.insertVec("a Neighbour under the current model", 1, 0)
	res := e.search(Query{RecordID: anchor, ProjectKey: "proj-a", Limit: 10})
	if len(res.Hits) != 0 || res.Reason != ReasonNoRecordVector {
		t.Fatalf("result = %+v, want no hits and reason %q", res, ReasonNoRecordVector)
	}
}

func TestRecordQueryWhenTheEmbedderFails(t *testing.T) {
	e := newHybridEnv(t, &fakeEmbedder{err: errors.New("runtime broken")})
	anchor := e.insertVec("the anchor", 1, 0)
	e.insertVec("a Neighbour", 1, 0)
	res := e.search(Query{RecordID: anchor, ProjectKey: "proj-a", Limit: 10})
	if want := "embeddings unavailable: runtime broken"; len(res.Hits) != 0 || res.Reason != want {
		t.Fatalf("result = %+v, want no hits and reason %q", res, want)
	}
}

// An Embedder that reports embeddings switched off (the daemon's, after
// `embed --off`) reads as "embeddings off", the same as having none, for
// text and Record queries alike.
func TestEmbedderThatIsSwitchedOffReadsAsEmbeddingsOff(t *testing.T) {
	e := newHybridEnv(t, &fakeEmbedder{err: fmt.Errorf("daemon: %w", ErrEmbeddingsOff)})
	hit := e.insertVec("quokka", 1, 0)
	res := e.search(Query{Text: "quokka", ProjectKey: "proj-a", Limit: 10})
	if got := ids(res.Hits); !slices.Equal(got, []string{hit}) {
		t.Fatalf("hits = %v, want [%s]", got, hit)
	}
	if res.Reason != ReasonEmbeddingsOff {
		t.Errorf("text query Reason = %q, want %q", res.Reason, ReasonEmbeddingsOff)
	}
	if res := e.search(Query{RecordID: hit, ProjectKey: "proj-a", Limit: 10}); res.Reason != ReasonEmbeddingsOff {
		t.Errorf("Record query Reason = %q, want %q", res.Reason, ReasonEmbeddingsOff)
	}
}
