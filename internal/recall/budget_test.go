package recall

import (
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/store"
)

// The budget cut picks survivors by priority, direct hits first, then
// edge-reached items by fewest hops, and renders the survivors newest first
// by sequence: a widened item never pushes out a direct hit, however much
// newer it is.
func TestBuildBudgetKeepsDirectHitsFirstThenFewestHopsInSequenceOrder(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")

	older := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the older direct hit")
	oneHop := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "one hop from the older hit")
	twoHops := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "two hops from the older hit")
	newer := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the newer direct hit")
	mustLinkEdge(t, st, older, oneHop, store.EdgeInforms, sid)
	mustLinkEdge(t, st, oneHop, twoHops, store.EdgeInforms, sid)
	searcher := fakeSearcher{ids: []string{newer, older}}
	anchor := TextAnchor("proj-a", "hit")

	whole, err := Build(st, searcher, anchor, AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := itemIDs(whole.Items), []string{newer, twoHops, oneHop, older}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unbudgeted items = %v, want %v", got, want)
	}
	// rendering is what Build renders for the items ids, in the order given.
	rendering := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = renderItem(itemByID(t, whole.Items, id), AltitudeSummary)
		}
		return strings.Join(parts, "\n\n")
	}

	for _, survivors := range [][]string{
		{newer, oneHop, older}, // the 2-hop item, between two survivors, is cut first
		{newer, older},         // then the 1-hop one: no widened item pushes out a direct hit
		{newer},                // then the older direct hit
	} {
		want := rendering(survivors...)
		res, err := Build(st, searcher, anchor, AltitudeSummary, block.EstimateTokens(want), nil)
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
