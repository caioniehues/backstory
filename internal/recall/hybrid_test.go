package recall

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

const hybridFingerprint = "sha256:recall|tmpl1|vulkan|2"

// stubEmbedder embeds every query as the same vector, or fails with err.
type stubEmbedder struct{ err error }

func (s stubEmbedder) Fingerprint(context.Context) (string, error) {
	return hybridFingerprint, s.err
}

func (s stubEmbedder) EmbedQuery(context.Context, string) (string, []float32, error) {
	if s.err != nil {
		return "", nil, s.err
	}
	return hybridFingerprint, []float32{1, 0}, nil
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func mustEmbed(t *testing.T, st *store.Store, id string, vec ...float32) {
	t.Helper()
	if ok, err := st.UpsertRecordEmbedding(id, hybridFingerprint, vec); err != nil || !ok {
		t.Fatalf("UpsertRecordEmbedding(%s) = %v, %v", id, ok, err)
	}
}

// A text anchor's recall is hybrid when the Searcher has an Embedder: a
// Record found only by meaning shows up beside the word match, and the
// notice carries the Searcher's reason, whichever it is.
func TestBuildTextAnchorNoticeCarriesTheVectorSidesReason(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")
	word := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "drop the memoisation layer")
	meaning := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "removed the cache")
	mustEmbed(t, st, word, 0, 1)
	mustEmbed(t, st, meaning, 1, 0)

	build := func(emb search.Embedder) Result {
		t.Helper()
		res, err := Build(st, search.New(st, search.WithEmbedder(emb), search.WithCosineFloor(0.5)),
			TextAnchor("proj-a", "memoisation"), AltitudeFull, testBudget, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return res
	}

	res := build(stubEmbedder{})
	if got, want := itemIDs(res.Items), []string{word, meaning}; !sameSet(got, want) {
		t.Fatalf("hybrid items = %v, want %v in any order", got, want)
	}
	if res.Notice != "" {
		t.Errorf("hybrid Notice = %q, want none (every Record embedded)", res.Notice)
	}

	res = build(stubEmbedder{err: errors.New("daemon unreachable")})
	if got := itemIDs(res.Items); !slices.Equal(got, []string{word}) {
		t.Fatalf("fallback items = %v, want [%s]", got, word)
	}
	if want := "embeddings unavailable: daemon unreachable"; res.Notice != want {
		t.Errorf("fallback Notice = %q, want %q", res.Notice, want)
	}

	mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "not embedded yet")
	if res = build(stubEmbedder{}); res.Notice != "embeddings partial: 1 Record not embedded yet" {
		t.Errorf("partial Notice = %q, want the partial reason", res.Notice)
	}
}
