package graph

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// fakeSearcher answers each Record query with the Neighbours chosen for
// that Record and a fixed reason, and keeps every query it was sent.
type fakeSearcher struct {
	neighbours map[string][]search.Hit
	reason     string
	queries    []search.Query
}

func (s *fakeSearcher) Search(_ context.Context, q search.Query) (search.Result, error) {
	s.queries = append(s.queries, q)
	return search.Result{Hits: s.neighbours[q.RecordID], Reason: s.reason}, nil
}

// near is a vector Hit on id at similarity sim.
func near(id string, sim float64) search.Hit {
	return search.Hit{RecordID: id, Via: search.ViaVector, Similarity: sim}
}

// walkWith runs Walk from seeds with limits through searcher.
func (f *fixture) walkWith(searcher search.Searcher, limits Limits, seeds ...string) Graph {
	f.t.Helper()
	g, err := Walk(context.Background(), Source{Store: f.st, Searcher: searcher}, seeds, limits)
	if err != nil {
		f.t.Fatalf("Walk: %v", err)
	}
	return g
}

// neighbourLimits asks for NeighboursPerSeed Neighbours from the fixture's
// Project, and no declared edges.
func (f *fixture) neighbourLimits() Limits {
	return Limits{Neighbours: NeighboursPerSeed, Scope: Scope{ProjectKey: f.project}}
}

// A seed's Neighbours are the closest NeighboursPerSeed Records its Record
// query returns, each reached as a Neighbour of the seed with its
// similarity, and a neighbour edge joins each to the seed.
func TestWalkAddsTheTopNeighboursOfEachSeed(t *testing.T) {
	f := newFixture(t)
	seed := f.record(store.KindDecision, "the seed")
	var hits []search.Hit
	for i := 0; i < NeighboursPerSeed+2; i++ {
		id := f.record(store.KindNote, "close in meaning")
		// Out of order, so the walker has to pick the closest itself.
		hits = append(hits, near(id, 0.6+float64((i*3)%(NeighboursPerSeed+2))/100))
	}
	fake := &fakeSearcher{neighbours: map[string][]search.Hit{seed: hits}}

	g := f.walkWith(fake, f.neighbourLimits(), seed)

	sorted := append([]search.Hit(nil), hits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Similarity > sorted[j].Similarity })
	want := map[string]float64{}
	for _, h := range sorted[:NeighboursPerSeed] {
		want[h.RecordID] = h.Similarity
	}
	if len(g.Nodes) != NeighboursPerSeed+1 {
		t.Fatalf("nodes = %v, want the seed plus its %d closest", nodeIDs(g), NeighboursPerSeed)
	}
	for id, sim := range want {
		r := node(t, g, id).Reached
		if r == nil || *r != (Reached{Via: ViaNeighbour, From: seed, Similarity: sim}) {
			t.Errorf("reached of %s = %+v, want Neighbour of the seed at %.2f", id, r, sim)
		}
	}
	if len(g.NeighbourEdges) != NeighboursPerSeed {
		t.Fatalf("neighbour edges = %+v, want %d", g.NeighbourEdges, NeighboursPerSeed)
	}
	for _, e := range g.NeighbourEdges {
		if e.From != seed || want[e.To] != e.Similarity {
			t.Errorf("neighbour edge %+v, want from the seed to a top Neighbour, with its similarity", e)
		}
	}
	if len(g.Edges) != 0 {
		t.Errorf("declared edges = %+v, want none: a neighbour edge is not a declared one", g.Edges)
	}
	if stored, err := f.st.EdgesTouching(seed); err != nil || len(stored) != 0 {
		t.Errorf("stored edges of the seed = %+v, %v, want none: neighbour edges are never stored", stored, err)
	}
	if len(fake.queries) != 1 {
		t.Fatalf("queries = %+v, want one", fake.queries)
	}
	if q := fake.queries[0]; q.RecordID != seed || q.ProjectKey != f.project || !q.CurrentOnly || q.Limit != NeighboursPerSeed {
		t.Errorf("query = %+v, want a current-only Record query on the seed in %s for %d", q, f.project, NeighboursPerSeed)
	}
}

// A Neighbour must be at least search.CosineFloor close to its seed,
// whatever the Searcher returns.
func TestWalkDropsNeighboursBelowTheCosineFloor(t *testing.T) {
	f := newFixture(t)
	seed := f.record(store.KindDecision, "the seed")
	atFloor := f.record(store.KindNote, "just close enough")
	below := f.record(store.KindNote, "not close enough")
	fake := &fakeSearcher{neighbours: map[string][]search.Hit{
		seed: {near(atFloor, search.CosineFloor), near(below, search.CosineFloor-0.01)},
	}}

	g := f.walkWith(fake, f.neighbourLimits(), seed)

	if want := []string{atFloor, seed}; !sameIDs(nodeIDs(g), want) {
		t.Errorf("nodes = %v, want %v (not %s, below the floor)", nodeIDs(g), want, below)
	}
	if len(g.NeighbourEdges) != 1 || g.NeighbourEdges[0].To != atFloor {
		t.Errorf("neighbour edges = %+v, want only the one to %s", g.NeighbourEdges, atFloor)
	}
}

// Neighbours come only from inside the caller's scope: its Project, or a
// Work location's scope (its repo key plus its Sessions), whatever the
// Searcher returns. Declared edges still cross scope.
func TestWalkKeepsNeighboursInsideTheCallersScope(t *testing.T) {
	f := newFixture(t)
	elsewhere := f.startSession("proj-b")
	homed := f.startSession("ws-home")
	seed := f.record(store.KindDecision, "the seed")
	inProject := f.record(store.KindNote, "same Project")
	otherProject := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindNote, Text: "another Project",
		SessionID: elsewhere, ProjectKey: "proj-b",
	})
	workspaceHomed := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: "homed under the Workspace",
		SessionID: homed, ProjectKey: "ws-home",
	})
	linked := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindNote, Text: "declared, in another Project",
		SessionID: elsewhere, ProjectKey: "proj-b",
	})
	f.link(seed, linked, store.EdgeInforms)
	fake := &fakeSearcher{neighbours: map[string][]search.Hit{
		seed: {near(otherProject, 0.9), near(workspaceHomed, 0.8), near(inProject, 0.7)},
	}}

	limits := f.neighbourLimits()
	limits.Hops = 1
	g := f.walkWith(fake, limits, seed)
	if want := []string{linked, inProject, seed}; !sameIDs(nodeIDs(g), want) {
		t.Errorf("Project scope nodes = %v, want %v", nodeIDs(g), want)
	}

	ls := store.LocationScope{RepoKey: f.project, SessionIDs: map[string]bool{homed: true}}
	g = f.walkWith(fake, Limits{Neighbours: NeighboursPerSeed, Scope: Scope{Location: &ls}}, seed)
	if want := []string{workspaceHomed, inProject, seed}; !sameIDs(nodeIDs(g), want) {
		t.Errorf("location scope nodes = %v, want %v (not %s, in another Project)", nodeIDs(g), want, otherProject)
	}
	if q := fake.queries[len(fake.queries)-1]; q.Scope != &ls {
		t.Errorf("query scope = %v, want the location scope", q.Scope)
	}
}

// Only Current Records and live Drafts are Neighbours, whatever the
// Searcher returns: never a Tombstoned, superseded or expired one, nor a
// confirm Record folded into its target.
func TestWalkNeighboursAreCurrentRecordsAndLiveDrafts(t *testing.T) {
	f := newFixture(t)
	seed := f.record(store.KindDecision, "the seed")
	current := f.record(store.KindNote, "current")
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	liveDraft := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: "a live guess", ExpiresAt: &future,
	})
	expiredDraft := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: "an expired guess", ExpiresAt: &past,
	})
	forgotten := f.record(store.KindNote, "Tombstoned")
	if err := f.st.TombstoneRecord(forgotten, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatalf("TombstoneRecord: %v", err)
	}
	old := f.record(store.KindDecision, "superseded")
	f.link(f.record(store.KindDecision, "the newer one"), old, store.EdgeSupersedes)
	affirm := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmAffirm, RecordID: current, SessionID: f.session})

	fake := &fakeSearcher{neighbours: map[string][]search.Hit{seed: {
		near(forgotten, 0.95), near(old, 0.9), near(expiredDraft, 0.85), near(affirm, 0.8),
		near(liveDraft, 0.7), near(current, 0.6),
	}}}

	g := f.walkWith(fake, f.neighbourLimits(), seed)

	if want := []string{liveDraft, current, seed}; !sameIDs(nodeIDs(g), want) {
		t.Errorf("nodes = %v, want %v: not Tombstoned %s, superseded %s, expired %s or affirm %s",
			nodeIDs(g), want, forgotten, old, expiredDraft, affirm)
	}
	if got := node(t, g, liveDraft).Status; got != StatusDraft {
		t.Errorf("live Draft's status = %q, want %q", got, StatusDraft)
	}
	if len(g.NeighbourEdges) != 2 {
		t.Errorf("neighbour edges = %+v, want the two to %s and %s", g.NeighbourEdges, liveDraft, current)
	}
}

// Only seeds have Neighbours: no Record query for an edge-reached node or
// a Neighbour, and a Neighbour's declared edges aren't followed. A Record
// already reached by an edge, or a seed, keeps how it was reached and only
// gains the neighbour edge; one close to two seeds is reached from the
// closer.
func TestWalkAddsNeighboursOfSeedsOnlyAndNeverWalksThemFurther(t *testing.T) {
	f := newFixture(t)
	seedA := f.record(store.KindDecision, "seed A")
	seedB := f.record(store.KindDecision, "seed B")
	linked := f.record(store.KindNote, "one declared edge from A")
	f.link(seedA, linked, store.EdgeInforms)
	shared := f.record(store.KindNote, "close to both seeds")
	beyond := f.record(store.KindNote, "a declared edge from the Neighbour")
	f.link(shared, beyond, store.EdgeInforms)
	fake := &fakeSearcher{neighbours: map[string][]search.Hit{
		seedA:  {near(shared, 0.7), near(linked, 0.8), near(seedB, 0.75)},
		seedB:  {near(shared, 0.9), near(seedA, 0.75)},
		linked: {near(f.record(store.KindNote, "close to the edge-reached node"), 0.9)},
		shared: {near(f.record(store.KindNote, "close to the Neighbour"), 0.9)},
	}}

	limits := f.neighbourLimits()
	limits.Hops = DefaultHops
	g := f.walkWith(fake, limits, seedA, seedB)

	if want := []string{shared, linked, seedB, seedA}; !sameIDs(nodeIDs(g), want) {
		t.Fatalf("nodes = %v, want %v (not beyond %s)", nodeIDs(g), want, beyond)
	}
	var queried []string
	for _, q := range fake.queries {
		queried = append(queried, q.RecordID)
	}
	sort.Strings(queried)
	want := []string{seedA, seedB}
	sort.Strings(want)
	if !sameIDs(queried, want) {
		t.Errorf("Record queries on %v, want only the seeds %v", queried, want)
	}
	if r := node(t, g, shared).Reached; r == nil || *r != (Reached{Via: ViaNeighbour, From: seedB, Similarity: 0.9}) {
		t.Errorf("shared's reached = %+v, want Neighbour of the closer seed B at 0.9", r)
	}
	if r := node(t, g, linked).Reached; r == nil || r.Via != ViaEdge {
		t.Errorf("linked's reached = %+v, want its declared edge", r)
	}
	if r := node(t, g, seedB).Reached; r != nil {
		t.Errorf("seed B's reached = %+v, want nil", r)
	}
	if len(g.NeighbourEdges) != 4 {
		t.Errorf("neighbour edges = %+v, want A-shared, A-linked, A-B and B-shared, each once", g.NeighbourEdges)
	}
}

// With no vector or runtime there are no Neighbours and the Graph carries
// the Searcher's reason. Embeddings off or unavailable stops the Record
// queries at the first seed; a partial reason doesn't, and is given once.
func TestWalkCarriesTheSearchersReasonForMissingNeighbours(t *testing.T) {
	f := newFixture(t)
	seedA := f.record(store.KindDecision, "seed A")
	seedB := f.record(store.KindDecision, "seed B")
	close := f.record(store.KindNote, "close to seed B")

	for _, reason := range []string{search.ReasonEmbeddingsOff, search.ReasonUnavailablePrefix + "daemon unreachable"} {
		fake := &fakeSearcher{reason: reason}
		g := f.walkWith(fake, f.neighbourLimits(), seedA, seedB)
		if g.Reason != reason {
			t.Errorf("Reason = %q, want %q", g.Reason, reason)
		}
		if len(g.Nodes) != 2 || len(g.NeighbourEdges) != 0 {
			t.Errorf("%s: nodes %v, neighbour edges %+v, want the seeds alone", reason, nodeIDs(g), g.NeighbourEdges)
		}
		if len(fake.queries) != 1 {
			t.Errorf("%s: %d Record queries, want 1", reason, len(fake.queries))
		}
	}

	partial := search.ReasonPartialPrefix + "1 Record not embedded yet"
	fake := &fakeSearcher{reason: partial, neighbours: map[string][]search.Hit{seedB: {near(close, 0.8)}}}
	g := f.walkWith(fake, f.neighbourLimits(), seedA, seedB)
	if g.Reason != partial || len(fake.queries) != 2 {
		t.Errorf("Reason %q after %d queries, want %q after 2", g.Reason, len(fake.queries), partial)
	}
	if r := node(t, g, close).Reached; r == nil || r.Via != ViaNeighbour {
		t.Errorf("reached = %+v, want a Neighbour despite the partial reason", r)
	}

	if g := f.walk(f.neighbourLimits(), seedA); g.Reason != search.ReasonEmbeddingsOff {
		t.Errorf("Reason with no Embedder = %q, want %q", g.Reason, search.ReasonEmbeddingsOff)
	}
	if g := f.walkWith(&fakeSearcher{reason: "unasked"}, Limits{Hops: 1}, seedA); g.Reason != "" {
		t.Errorf("Reason without Neighbours asked for = %q, want none", g.Reason)
	}
}

// Up to MaxNeighbourSeeds seeds, each gets its own Record query. Above
// it, the walker makes none (the pairwise path is #46's).
func TestWalkMakesPerSeedRecordQueriesUpToTheSeedCap(t *testing.T) {
	f := newFixture(t)
	var seeds []string
	for i := 0; i <= MaxNeighbourSeeds; i++ {
		seeds = append(seeds, f.record(store.KindNote, "a seed"))
	}

	fake := &fakeSearcher{}
	f.walkWith(fake, f.neighbourLimits(), seeds[:MaxNeighbourSeeds]...)
	if len(fake.queries) != MaxNeighbourSeeds {
		t.Errorf("%d seeds: %d Record queries, want one each", MaxNeighbourSeeds, len(fake.queries))
	}

	fake = &fakeSearcher{}
	f.walkWith(fake, f.neighbourLimits(), seeds...)
	if len(fake.queries) != 0 {
		t.Errorf("%d seeds: %d Record queries, want none above the cap", len(seeds), len(fake.queries))
	}
}
