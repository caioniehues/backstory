package recall

import (
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

func mustTombstone(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.TombstoneRecord(id, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatalf("TombstoneRecord: %v", err)
	}
}

func itemByID(t *testing.T, items []Item, id string) Item {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %s among %v", id, itemIDs(items))
	return Item{}
}

// A record anchor's items come from the Ledger walk: a Tombstoned Record is
// dropped, though the walk still goes through it, a Done/Affirm/Promote
// confirm Record isn't an item, and each item reached by an edge says how.
func TestBuildRecordAnchorWalksTheLedgerGraphAndDropsTombstoned(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	sid := mustStartSession(t, st, "proj-a")

	root := mustInsertRecord(t, st, sid, "proj-a", store.KindDecision, "the anchor")
	informed := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "informed by the anchor")
	forgotten := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "forgotten text")
	beyond := mustInsertRecord(t, st, sid, "proj-a", store.KindNote, "reached through the forgotten one")
	mustLinkEdge(t, st, root, informed, store.EdgeInforms, sid)
	mustLinkEdge(t, st, forgotten, root, store.EdgeCaused, sid)
	mustLinkEdge(t, st, forgotten, beyond, store.EdgeInforms, sid)
	mustTombstone(t, st, forgotten)
	affirm, err := st.Confirm(store.ConfirmParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmAffirm, RecordID: root, SessionID: sid, ProjectKey: "proj-a",
	})
	if err != nil {
		t.Fatalf("Confirm affirm: %v", err)
	}

	res, err := Build(st, search.New(st), RecordAnchor(root), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := itemIDs(res.Items), []string{beyond, informed, root}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("items = %v, want %v (no Tombstoned %s, no affirm %s)", got, want, forgotten, affirm)
	}
	if strings.Contains(res.Rendered, "forgotten text") || strings.Contains(res.Rendered, "tombstoned") {
		t.Errorf("rendered shows the Tombstoned Record:\n%s", res.Rendered)
	}

	if r := itemByID(t, res.Items, root).Reached; r != nil {
		t.Errorf("anchor's Reached = %+v, want nil", r)
	}
	r := itemByID(t, res.Items, informed).Reached
	if r == nil || *r != (graph.Reached{Via: graph.ViaEdge, Type: store.EdgeInforms, Direction: graph.DirectionOut, From: root, Hops: 1}) {
		t.Errorf("informed's Reached = %+v, want out along informs from the anchor, 1 hop", r)
	}
	if cs := itemByID(t, res.Items, root).ConfirmedBy; len(cs) != 1 || cs[0].ID != affirm || cs[0].Type != store.ConfirmAffirm {
		t.Errorf("anchor's ConfirmedBy = %+v, want the affirm", cs)
	}

	wantLines := map[string]string{
		informed: "← informs `" + shortID(root) + "`",
		beyond:   "← informs `" + shortID(forgotten) + "`",
	}
	for id, line := range wantLines {
		if got := reachedLine(t, res.Rendered, id); got != line {
			t.Errorf("reached line of %s = %q, want %q", shortID(id), got, line)
		}
	}
	if got := reachedLine(t, res.Rendered, root); got != "" {
		t.Errorf("anchor has reached line %q, want none", got)
	}
}

// reachedLine is the line after id's header in a summary rendering when it
// is a reached line, else "".
func reachedLine(t *testing.T, rendered, id string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, shortID(id)+" · ") {
			if i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "← ") || strings.HasPrefix(lines[i+1], "→ ") || strings.HasPrefix(lines[i+1], "Neighbour of ")) {
				return lines[i+1]
			}
			return ""
		}
	}
	t.Fatalf("no header for %s in:\n%s", shortID(id), rendered)
	return ""
}

// The reached line points the edge's way relative to the item, and an
// item from a Project other than the anchor's is labelled with it, on one
// line at headline altitude.
func TestBuildRecordAnchorLabelsDirectionAndOtherProject(t *testing.T) {
	st := newTestStore(t)
	mustUpsertProject(t, st, "proj-a")
	mustUpsertProject(t, st, "proj-b")
	sa := mustStartSession(t, st, "proj-a")
	sb := mustStartSession(t, st, "proj-b")

	root := mustInsertRecord(t, st, sa, "proj-a", store.KindDecision, "the anchor")
	newer := mustInsertRecord(t, st, sb, "proj-b", store.KindDecision, "supersedes the anchor, elsewhere")
	mustLinkEdge(t, st, newer, root, store.EdgeSupersedes, sb)

	res, err := Build(st, search.New(st), RecordAnchor(root), AltitudeSummary, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := reachedLine(t, res.Rendered, newer), "→ supersedes `"+shortID(root)+"`"; got != want {
		t.Errorf("reached line = %q, want %q", got, want)
	}
	it := itemByID(t, res.Items, newer)
	if it.ProjectKey != "proj-b" || !it.OtherProject {
		t.Errorf("item ProjectKey %q OtherProject %v, want proj-b and true", it.ProjectKey, it.OtherProject)
	}
	if a := itemByID(t, res.Items, root); a.ProjectKey != "proj-a" || a.OtherProject {
		t.Errorf("anchor ProjectKey %q OtherProject %v, want proj-a and false", a.ProjectKey, a.OtherProject)
	}

	head, err := Build(st, search.New(st), RecordAnchor(root), AltitudeHeadline, testBudget, nil)
	if err != nil {
		t.Fatalf("Build headline: %v", err)
	}
	for _, line := range strings.Split(head.Rendered, "\n") {
		other := strings.HasPrefix(line, shortID(newer))
		if other && (!strings.Contains(line, " · project proj-b") || !strings.Contains(line, " · → supersedes `"+shortID(root)+"`")) {
			t.Errorf("headline %q lacks its Project label or reached segment", line)
		}
		if !other && strings.Contains(line, "project ") {
			t.Errorf("headline %q is labelled, but it is in the anchor's Project", line)
		}
	}
}

// Every anchor's Handoff items carry handoff's status, and every item its
// confirmations, whether or not the walk found it.
func TestBuildItemsCarryHandoffStatusAndConfirmations(t *testing.T) {
	st, sessionID := newFreshnessFixture(t)
	h := mustInsertHandoffAbout(t, st, sessionID, []string{"a.go"})
	note := mustInsertNoteAbout(t, st, sessionID, []string{"a.go"})
	mustAffirm(t, st, sessionID, h.ID)

	item := buildHandoffItem(t, st, h.ID)
	if item.Handoff == nil || item.Handoff.Done || item.Handoff.PossiblyStale() {
		t.Errorf("affirmed Handoff's Handoff = %+v, want neither done nor possibly stale", item.Handoff)
	}
	if len(item.ConfirmedBy) != 1 || item.ConfirmedBy[0].Type != store.ConfirmAffirm {
		t.Errorf("affirmed Handoff's ConfirmedBy = %+v, want one affirm", item.ConfirmedBy)
	}
	if item.ProjectKey != freshnessProjectKey {
		t.Errorf("ProjectKey = %q, want %q", item.ProjectKey, freshnessProjectKey)
	}

	res, err := Build(st, search.New(st), ProjectAnchor(freshnessProjectKey), AltitudeFull, testBudget, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := itemByID(t, res.Items, note); n.Handoff != nil || n.ConfirmedBy == nil || len(n.ConfirmedBy) != 0 {
		t.Errorf("note's Handoff %+v ConfirmedBy %#v, want nil and empty", n.Handoff, n.ConfirmedBy)
	}

	// A superseded Handoff still carries its status.
	newer := mustInsertHandoffAbout(t, st, sessionID, []string{"a.go"})
	mustLinkEdge(t, st, newer.ID, h.ID, store.EdgeSupersedes, sessionID)
	if old := buildHandoffItem(t, st, h.ID); old.Status != StatusSuperseded || old.Handoff == nil {
		t.Errorf("superseded Handoff: Status %q Handoff %+v, want superseded with a status", old.Status, old.Handoff)
	}
}
