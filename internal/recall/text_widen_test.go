package recall

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// A text anchor widens along declared edges from its top textAnchorSeeds
// hits by search rank, and from no other hit. Every hit stays a direct hit,
// even one a top hit's edge reaches; a widened item says how it was reached
// and, from another Project, which one. Items render newest first by
// sequence, whatever the search rank.
func TestBuildTextAnchorWidensAlongEdgesFromItsTopHitsOnly(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	mustUpsertProject(t, st, "proj-b")
	sa := mustStartSession(t, st, "proj-a")
	sb := mustStartSession(t, st, "proj-b")

	widened := mustInsertRecord(t, st, sb, "proj-b", store.KindDecision, "informed by the best hit, elsewhere")
	forgotten := mustInsertRecord(t, st, sa, "proj-a", store.KindNote, "forgotten, though the best hit informs it")
	hits := make([]string, textAnchorSeeds+2)
	for i := range hits {
		hits[i] = mustInsertRecord(t, st, sa, "proj-a", store.KindNote, fmt.Sprintf("hit %d", i))
	}
	unwidened := mustInsertRecord(t, st, sa, "proj-a", store.KindNote, "informed only by a hit below the top")

	// Search rank is the order hits were written in, the oldest first.
	top, belowTop := hits[:textAnchorSeeds], hits[textAnchorSeeds:]
	mustLinkEdge(t, st, top[0], widened, store.EdgeInforms, sa)
	mustLinkEdge(t, st, top[0], forgotten, store.EdgeInforms, sa)
	mustTombstone(t, st, forgotten)
	mustLinkEdge(t, st, belowTop[0], unwidened, store.EdgeInforms, sa)
	mustLinkEdge(t, st, top[1], belowTop[1], store.EdgeCaused, sa)

	res, err := Build(st, fakeSearcher{ids: hits}, TextAnchor("proj-a", "hit"), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var want []string
	for i := len(hits) - 1; i >= 0; i-- {
		want = append(want, hits[i])
	}
	want = append(want, widened)
	if strings.Contains(res.Rendered, "forgotten") {
		t.Errorf("rendered shows the Tombstoned Record:\n%s", res.Rendered)
	}
	if got := itemIDs(res.Items); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("items = %v, want %v", got, want)
	}

	for _, id := range hits {
		if it := itemByID(t, res.Items, id); it.Reached != nil || it.OtherProject {
			t.Errorf("hit %s: Reached %+v OtherProject %v, want a direct hit", shortID(id), it.Reached, it.OtherProject)
		}
		if got := reachedLine(t, res.Rendered, id); got != "" {
			t.Errorf("hit %s has reached line %q, want none", shortID(id), got)
		}
	}
	w := itemByID(t, res.Items, widened)
	if w.Reached == nil || *w.Reached != (graph.Reached{Via: graph.ViaEdge, Type: store.EdgeInforms, Direction: graph.DirectionOut, From: hits[0], Hops: 1}) {
		t.Errorf("widened Reached = %+v, want out along informs from the best hit, 1 hop", w.Reached)
	}
	if !w.OtherProject || w.ProjectKey != "proj-b" {
		t.Errorf("widened OtherProject %v ProjectKey %q, want true and proj-b", w.OtherProject, w.ProjectKey)
	}
	if !strings.Contains(res.Rendered, " · project proj-b\n") {
		t.Errorf("widened item's header lacks its Project label in:\n%s", res.Rendered)
	}
	if got, want := reachedLine(t, res.Rendered, widened), "← informs `"+shortID(top[0])+"`"; got != want {
		t.Errorf("widened reached line = %q, want %q", got, want)
	}
}

// A superseded Record is never a text anchor's direct hit: it is reachable
// only along an edge, and labelled. An expired Draft an edge reaches is
// left out.
func TestBuildTextAnchorReachesSupersededRecordsOnlyByEdges(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")

	old := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "wobble plan A")
	newer := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "wobble plan B")
	orphan := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "wobble plan C")
	replacement := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "a different approach")
	past := time.Now().Add(-time.Hour)
	expired, err := st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: "an expired guess",
		SessionID: sid, ProjectKey: "proj-a", ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("InsertRecord expired Draft: %v", err)
	}
	mustLinkEdge(t, st, newer, old, store.EdgeSupersedes, sid)
	mustLinkEdge(t, st, replacement, orphan, store.EdgeSupersedes, sid)
	mustLinkEdge(t, st, newer, expired, store.EdgeInforms, sid)

	res, err := Build(st, search.New(st), TextAnchor("proj-a", "wobble"), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := itemIDs(res.Items), []string{newer, old}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("items = %v, want %v (no superseded %s without its superseder, no expired Draft %s)", got, want, orphan, expired)
	}
	o := itemByID(t, res.Items, old)
	if o.Status != StatusSuperseded || o.SupersededByID != newer {
		t.Errorf("superseded item Status %q by %q, want superseded by %s", o.Status, o.SupersededByID, newer)
	}
	if got, want := reachedLine(t, res.Rendered, old), "← supersedes `"+shortID(newer)+"`"; got != want {
		t.Errorf("superseded reached line = %q, want %q", got, want)
	}
	if !strings.Contains(res.Rendered, shortID(old)+" · decision · agent-declared · superseded by "+shortID(newer)) {
		t.Errorf("superseded item not labelled in:\n%s", res.Rendered)
	}
}

// A query naming the handoff kind adds the location's handoffs, but not a
// superseded one, and the no-match fallback never picks one either.
func TestBuildTextAnchorHandoffsLeaveOutSupersededOnes(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")
	kept := insertHandoff(t, st, sid, "an older handoff, still current", "")
	dropped := insertHandoff(t, st, sid, "the newest handoff, superseded", "")
	replacement := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "replaces the newest handoff")
	mustLinkEdge(t, st, replacement, dropped, store.EdgeSupersedes, sid)

	for _, q := range []string{"resume", "zzqx nonexistent"} {
		res, err := Build(st, search.New(st), TextAnchor("proj-a", q), AltitudeSummary, testBudget, nil)
		if err != nil {
			t.Fatalf("%q: Build: %v", q, err)
		}
		if got := itemIDs(res.Items); len(got) != 1 || got[0] != kept {
			t.Errorf("%q: items = %v, want [%s] (not the superseded %s)", q, got, kept, dropped)
		}
	}
}
