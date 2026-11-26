package store

import (
	"math"
	"slices"
	"testing"
	"time"
)

// vectorIDs is the ids of hits, in order.
func vectorIDs(hits []VectorHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.RecordID
	}
	return out
}

func (e *searchEnv) nearest(query []float32, f SearchFilter) VectorScan {
	e.t.Helper()
	scan, err := e.s.NearestVectors(testModel, query, f)
	if err != nil {
		e.t.Fatalf("NearestVectors: %v", err)
	}
	return scan
}

// The filters narrow the candidates before the limit, exactly as for the
// text match: closer vectors outside the filter never take the place of the
// one inside it.
func TestNearestVectorsFiltersBeforeRanking(t *testing.T) {
	e := newSearchEnv(t)
	exact := []float32{1, 0}
	for i := 0; i < 5; i++ {
		mustUpsertVector(t, e.s, e.insert("proj-b", KindDecision, "other project", ""), testModel, exact)
		mustUpsertVector(t, e.s, e.insert("proj-a", KindNote, "other kind", ""), testModel, exact)
	}
	want := e.insert("proj-a", KindDecision, "the one", "")
	mustUpsertVector(t, e.s, want, testModel, []float32{0.6, 0.8})
	old := e.insert("proj-a", KindDecision, "superseded", "")
	mustUpsertVector(t, e.s, old, testModel, exact)
	repl := e.insert("proj-a", KindDecision, "replacement", "")
	mustUpsertVector(t, e.s, repl, testModel, []float32{0, 1})
	if err := e.s.LinkEdge(repl, old, EdgeSupersedes, "human"); err != nil {
		t.Fatal(err)
	}
	ls, err := e.s.LocationScopeForKey("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	decisions := []RecordKind{KindDecision}
	for name, f := range map[string]SearchFilter{
		"project": {ProjectKey: "proj-a", Kinds: decisions, CurrentOnly: true, Limit: 1},
		"scope":   {Scope: &ls, Kinds: decisions, CurrentOnly: true, Limit: 1},
	} {
		hits := e.nearest(exact, f).Hits
		if !slices.Equal(vectorIDs(hits), []string{want}) {
			t.Fatalf("%s: hits = %v, want [%s]", name, vectorIDs(hits), want)
		}
		if math.Abs(hits[0].Similarity-0.6) > 1e-6 {
			t.Errorf("%s: similarity = %v, want 0.6 (cosine of (1,0) and (0.6,0.8))", name, hits[0].Similarity)
		}
	}

	all := e.nearest(exact, SearchFilter{ProjectKey: "proj-a", Kinds: decisions, Limit: 3}).Hits
	if !slices.Equal(vectorIDs(all), []string{old, want, repl}) {
		t.Fatalf("without CurrentOnly hits = %v, want [%s %s %s] (closest first)", vectorIDs(all), old, want, repl)
	}
}

func TestNearestVectorsNeverReturnsTombstoned(t *testing.T) {
	e := newSearchEnv(t)
	gone := e.insert("proj-a", KindDecision, "forgotten", "")
	mustUpsertVector(t, e.s, gone, testModel, []float32{1, 0})
	kept := e.insert("proj-a", KindDecision, "kept", "")
	mustUpsertVector(t, e.s, kept, testModel, []float32{0.5, 0.5})
	if err := e.s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	got := vectorIDs(e.nearest([]float32{1, 0}, SearchFilter{ProjectKey: "proj-a", Limit: 10}).Hits)
	if !slices.Equal(got, []string{kept}) {
		t.Fatalf("hits = %v, want [%s]", got, kept)
	}
}

// Vectors from another model, or of another dimension, are never compared
// with the query: their cosine would mean nothing.
func TestNearestVectorsComparesOnlyTheQueryFingerprintAndDim(t *testing.T) {
	e := newSearchEnv(t)
	same := e.insert("proj-a", KindNote, "current model", "")
	mustUpsertVector(t, e.s, same, testModel, []float32{0, 0, 1})
	otherModel := e.insert("proj-a", KindNote, "old model", "")
	mustUpsertVector(t, e.s, otherModel, "sha256:old|tmpl1|vulkan|3", []float32{1, 0, 0})
	otherDim := e.insert("proj-a", KindNote, "wrong dim under the same name", "")
	mustUpsertVector(t, e.s, otherDim, testModel, []float32{1, 0, 0, 0})

	got := vectorIDs(e.nearest([]float32{1, 0, 0}, SearchFilter{ProjectKey: "proj-a", Limit: 10}).Hits)
	if !slices.Equal(got, []string{same}) {
		t.Fatalf("hits = %v, want only [%s]", got, same)
	}
}

// Unembedded counts the Records under the filter that would be embedded but
// have no vector for the query's fingerprint yet: what makes a result
// partial during a model changeover.
func TestNearestVectorsCountsUnembeddedUnderTheFilter(t *testing.T) {
	e := newSearchEnv(t)
	mustUpsertVector(t, e.s, e.insert("proj-a", KindDecision, "embedded", ""), testModel, []float32{1, 0, 0})
	mustUpsertVector(t, e.s, e.insert("proj-a", KindOutcome, "old model only", ""), "sha256:old|tmpl1|vulkan|3", []float32{1, 0, 0})
	e.insert("proj-a", KindHandoff, "never embedded", "next")
	e.insert("proj-a", KindClaim, "claims are never embedded", "")
	e.insert("proj-b", KindNote, "outside the filter", "")

	scan := e.nearest([]float32{1, 0, 0}, SearchFilter{ProjectKey: "proj-a", Limit: 10})
	if scan.Unembedded != 2 {
		t.Fatalf("Unembedded = %d, want 2 (the outcome on the old model and the handoff)", scan.Unembedded)
	}
	scan = e.nearest([]float32{1, 0, 0}, SearchFilter{ProjectKey: "proj-a", Kinds: []RecordKind{KindDecision, KindClaim}, Limit: 10})
	if scan.Unembedded != 0 {
		t.Fatalf("decisions and claims: Unembedded = %d, want 0", scan.Unembedded)
	}
}

func TestNearestVectorsLimit(t *testing.T) {
	e := newSearchEnv(t)
	mustUpsertVector(t, e.s, e.insert("proj-a", KindNote, "x", ""), testModel, []float32{1, 0, 0})
	if scan := e.nearest([]float32{1, 0, 0}, SearchFilter{ProjectKey: "proj-a"}); len(scan.Hits) != 0 {
		t.Fatalf("Limit 0: hits = %v, want none", vectorIDs(scan.Hits))
	}
}

// A Record query reads the Record's stored vector for the fingerprint, and
// finds none under another model.
func TestRecordVector(t *testing.T) {
	e := newSearchEnv(t)
	id := e.insert("proj-a", KindDecision, "has a vector", "")
	mustUpsertVector(t, e.s, id, testModel, []float32{0.5, -1, 2})

	vec, ok, err := e.s.RecordVector(id, testModel)
	if err != nil || !ok || !slices.Equal(vec, []float32{0.5, -1, 2}) {
		t.Fatalf("RecordVector = %v, %v, %v; want the stored vector", vec, ok, err)
	}
	for _, c := range []struct{ id, model string }{{id, "sha256:old|tmpl1|vulkan|3"}, {"no-such-record", testModel}} {
		if vec, ok, err := e.s.RecordVector(c.id, c.model); err != nil || ok || vec != nil {
			t.Errorf("RecordVector(%s, %s) = %v, %v, %v; want none", c.id, c.model, vec, ok, err)
		}
	}
}

// An expired Draft is filtered out of the vector scan before ranking, and
// isn't counted as waiting for a vector either.
func TestNearestVectorsExcludesExpiredDrafts(t *testing.T) {
	e := newSearchEnv(t)
	expired := e.insertDraft("expired guess", time.Now().Add(-time.Hour))
	mustUpsertVector(t, e.s, expired, testModel, []float32{1, 0})
	e.insertDraft("expired guess, no vector", time.Now().Add(-time.Hour))
	live := e.insertDraft("live guess", time.Now().Add(time.Hour))
	mustUpsertVector(t, e.s, live, testModel, []float32{0.6, 0.8})

	scan := e.nearest([]float32{1, 0}, SearchFilter{ProjectKey: "proj-a", Limit: 1})
	if !slices.Equal(vectorIDs(scan.Hits), []string{live}) {
		t.Fatalf("hits = %v, want [%s] (the closer expired Draft is never a hit)", vectorIDs(scan.Hits), live)
	}
	if scan.Unembedded != 0 {
		t.Errorf("Unembedded = %d, want 0 (an expired Draft is not waiting to be searched)", scan.Unembedded)
	}
}
