package recall

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// neighbourSearcher answers a text query with fixed hits and a Record
// query with the Neighbours chosen for that Record, each with reason, and
// counts the Record queries.
type neighbourSearcher struct {
	text          []string
	neighbours    map[string][]search.Hit
	reason        string
	recordQueries int
}

func (s *neighbourSearcher) Search(_ context.Context, q search.Query) (search.Result, error) {
	res := search.Result{Reason: s.reason}
	if q.RecordID != "" {
		s.recordQueries++
		res.Hits = s.neighbours[q.RecordID]
		return res, nil
	}
	for _, id := range s.text {
		res.Hits = append(res.Hits, search.Hit{RecordID: id, Via: search.ViaText})
	}
	return res, nil
}

func vectorHit(id string, sim float64) search.Hit {
	return search.Hit{RecordID: id, Via: search.ViaVector, Similarity: sim}
}

// A record anchor adds the anchor's Neighbours, each saying so with its
// similarity, on its own line or as a headline segment.
func TestBuildRecordAnchorShowsTheAnchorsNeighbours(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")
	root := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the anchor")
	close := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "close in meaning")
	fake := &neighbourSearcher{neighbours: map[string][]search.Hit{root: {vectorHit(close, 0.82)}}}

	res, err := Build(st, fake, RecordAnchor(root), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := itemIDs(res.Items); strings.Join(got, ",") != close+","+root {
		t.Fatalf("items = %v, want [%s %s]", got, close, root)
	}
	if r := itemByID(t, res.Items, close).Reached; r == nil || *r != (graph.Reached{Via: graph.ViaNeighbour, From: root, Similarity: 0.82}) {
		t.Errorf("Reached = %+v, want Neighbour of the anchor at 0.82", r)
	}
	want := "Neighbour of `" + shortID(root) + "` (0.82)"
	if got := reachedLine(t, res.Rendered, close); got != want {
		t.Errorf("reached line = %q, want %q", got, want)
	}
	if res.Notice != "" {
		t.Errorf("Notice = %q, want none", res.Notice)
	}

	head, err := Build(st, fake, RecordAnchor(root), AltitudeHeadline, testBudget, nil)
	if err != nil {
		t.Fatalf("Build headline: %v", err)
	}
	if !strings.Contains(head.Rendered, shortID(close)+" · decision · agent-declared · current · "+want+" · close in meaning") {
		t.Errorf("headline lacks the Neighbour segment:\n%s", head.Rendered)
	}
}

// With no vector or runtime a record anchor has no Neighbours and its
// notice carries the Searcher's reason; its declared edges still widen it.
func TestBuildRecordAnchorNoticeCarriesWhyThereAreNoNeighbours(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")
	root := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the anchor")
	linked := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "informed by the anchor")
	mustLinkEdge(t, st, root, linked, store.EdgeInforms, sid)

	for want, searcher := range map[string]search.Searcher{
		search.ReasonEmbeddingsOff:                   search.New(st),
		"embeddings unavailable: daemon unreachable": search.New(st, search.WithEmbedder(stubEmbedder{err: errors.New("daemon unreachable")})),
		search.ReasonNoRecordVector:                  search.New(st, search.WithEmbedder(stubEmbedder{})),
	} {
		res, err := Build(st, searcher, RecordAnchor(root), AltitudeSummary, testBudget, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := itemIDs(res.Items); strings.Join(got, ",") != linked+","+root {
			t.Errorf("items = %v, want [%s %s]", got, linked, root)
		}
		if res.Notice != want {
			t.Errorf("Notice = %q, want %q", res.Notice, want)
		}
	}
}

// A text anchor adds the Neighbours of its top hits, inside its scope, in
// sequence order with the hits, which all stay direct. It stays under the
// walker's seed cap however many hits it has, and the Searcher's reason
// is in the notice once.
func TestBuildTextAnchorShowsNeighboursOfItsTopHits(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	mustUpsertProject(t, st, "proj-b")
	sid := mustStartSession(t, st, "proj-a")
	other := mustStartSession(t, st, "proj-b")
	close := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "close to the best hit")
	elsewhere := mustInsertRecord(t, st, other, "proj-b", store.KindNote, "close, but in another Project")
	var hits []string
	for i := 0; i <= graph.MaxNeighbourSeeds+4; i++ {
		hits = append(hits, mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "matches the words"))
	}
	pastTop := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "close to a hit past the top ones")
	partial := search.ReasonPartialPrefix + "1 Record not embedded yet"
	fake := &neighbourSearcher{text: hits, reason: partial, neighbours: map[string][]search.Hit{
		hits[0]:                       {vectorHit(close, 0.9), vectorHit(elsewhere, 0.95)},
		hits[1]:                       {vectorHit(hits[2], 0.8)},
		hits[graph.MaxNeighbourSeeds]: {vectorHit(pastTop, 0.9)},
	}}

	res, err := Build(st, fake, TextAnchor("proj-a", "words"), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if fake.recordQueries == 0 || fake.recordQueries > graph.MaxNeighbourSeeds {
		t.Errorf("%d Record queries, want some, and at most the seed cap %d", fake.recordQueries, graph.MaxNeighbourSeeds)
	}
	ids := itemIDs(res.Items)
	if len(ids) != len(hits)+1 || ids[len(ids)-1] != close {
		t.Fatalf("items = %v, want the %d hits then, oldest, %s (not %s nor %s)", ids, len(hits), close, elsewhere, pastTop)
	}
	if r := itemByID(t, res.Items, close).Reached; r == nil || *r != (graph.Reached{Via: graph.ViaNeighbour, From: hits[0], Similarity: 0.9}) {
		t.Errorf("Reached = %+v, want Neighbour of the best hit at 0.9", r)
	}
	if got, want := reachedLine(t, res.Rendered, close), "Neighbour of `"+shortID(hits[0])+"` (0.90)"; got != want {
		t.Errorf("reached line = %q, want %q", got, want)
	}
	for _, h := range hits {
		if r := itemByID(t, res.Items, h).Reached; r != nil {
			t.Errorf("hit %s Reached = %+v, want nil: every hit stays direct", h, r)
		}
	}
	if res.Notice != partial {
		t.Errorf("Notice = %q, want %q once", res.Notice, partial)
	}
}

// A location-scoped text anchor's Neighbours come from its location's
// scope: a Record homed under the Workspace by one of its Sessions is
// inside it, another Project's is not.
func TestBuildTextAnchorNeighboursComeFromItsLocationScope(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	mustUpsertProject(t, st, "ws-home")
	mustUpsertProject(t, st, "proj-b")
	sid := mustStartSession(t, st, "proj-a")
	homedSession := mustStartSession(t, st, "ws-home")
	otherSession := mustStartSession(t, st, "proj-b")
	hit := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "matches the words")
	homed := mustInsertRecord(t, st, homedSession, "ws-home", store.KindHandoff, "homed under the Workspace")
	elsewhere := mustInsertRecord(t, st, otherSession, "proj-b", store.KindNote, "in another Project")
	fake := &neighbourSearcher{text: []string{hit}, neighbours: map[string][]search.Hit{
		hit: {vectorHit(elsewhere, 0.9), vectorHit(homed, 0.8)},
	}}
	ls := store.LocationScope{RepoKey: "proj-a", SessionIDs: map[string]bool{sid: true, homedSession: true}}

	res, err := Build(st, fake, TextAnchor("proj-a", "words").WithScope(ls), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := itemIDs(res.Items); strings.Join(got, ",") != homed+","+hit {
		t.Errorf("items = %v, want [%s %s] (not %s)", got, homed, hit, elsewhere)
	}
}

// The budget ranks Neighbours after edge-reached items, closer first, and
// still renders the survivors newest first by sequence.
func TestBuildBudgetRanksNeighboursAfterEdgesByCloseness(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")

	root := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the anchor")
	linked := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "informed by the anchor, and longer than either Neighbour")
	closer := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "the closer one")
	farther := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "the farther one")
	mustLinkEdge(t, st, root, linked, store.EdgeInforms, sid)
	fake := &neighbourSearcher{neighbours: map[string][]search.Hit{root: {vectorHit(farther, 0.6), vectorHit(closer, 0.9)}}}

	whole, err := Build(st, fake, RecordAnchor(root), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := itemIDs(whole.Items), []string{farther, closer, linked, root}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unbudgeted items = %v, want %v", got, want)
	}
	rendering := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = renderItem(itemByID(t, whole.Items, id), AltitudeSummary)
		}
		return strings.Join(parts, "\n\n")
	}

	for _, survivors := range [][]string{
		{closer, linked, root}, // the farther Neighbour, the newest item, is cut first
		{linked, root},         // then the closer one, before the edge-reached item
	} {
		want := rendering(survivors...)
		res, err := Build(st, fake, RecordAnchor(root), AltitudeSummary, block.EstimateTokens(want), nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := itemIDs(res.Items); strings.Join(got, ",") != strings.Join(survivors, ",") {
			t.Errorf("budget %d: items = %v, want %v", block.EstimateTokens(want), got, survivors)
		}
		if res.Rendered != want {
			t.Errorf("budget %d: rendered\n%s\nwant\n%s", block.EstimateTokens(want), res.Rendered, want)
		}
	}
}
