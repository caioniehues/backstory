package graph

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// fixture is a real temp store with one Project and one Session in it,
// for writing Records and edges through the store's own API.
type fixture struct {
	t       *testing.T
	st      *store.Store
	project string
	session string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, project: "proj-a"}
	f.session = f.startSession(f.project)
	return f
}

// startSession upserts project and starts a live Session in it.
func (f *fixture) startSession(project string) string {
	f.t.Helper()
	if err := f.st.UpsertProject(store.Project{Key: project, Toplevel: "/home/fixture/" + project, FirstSeen: time.Now()}); err != nil {
		f.t.Fatalf("UpsertProject: %v", err)
	}
	id, err := f.st.StartSession(store.StartSessionParams{
		Agent: "claude", CWD: "/home/fixture/" + project, ProjectKey: project, StartedAt: time.Now(), Origin: store.OriginLive,
	})
	if err != nil {
		f.t.Fatalf("StartSession: %v", err)
	}
	return id
}

// record writes an Agent's Record of kind in the fixture's Project.
func (f *fixture) record(kind store.RecordKind, text string) string {
	f.t.Helper()
	return f.insert(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityAgent}, Kind: kind, Text: text})
}

// insert writes p, defaulting its Session and Project to the fixture's.
func (f *fixture) insert(p store.InsertRecordParams) string {
	f.t.Helper()
	if p.SessionID == "" {
		p.SessionID = f.session
	}
	if p.ProjectKey == "" {
		p.ProjectKey = f.project
	}
	id, err := f.st.InsertRecord(p)
	if err != nil {
		f.t.Fatalf("InsertRecord: %v", err)
	}
	return id
}

// link declares from -typ-> to.
func (f *fixture) link(from, to string, typ store.EdgeType) {
	f.t.Helper()
	if err := f.st.LinkEdge(from, to, typ, f.session); err != nil {
		f.t.Fatalf("LinkEdge %s -%s-> %s: %v", from, typ, to, err)
	}
}

// walk runs Walk from seeds with limits and fails the test on an error.
func (f *fixture) walk(limits Limits, seeds ...string) Graph {
	f.t.Helper()
	g, err := Walk(context.Background(), Source{Store: f.st, Searcher: search.New(f.st)}, seeds, limits)
	if err != nil {
		f.t.Fatalf("Walk: %v", err)
	}
	return g
}

// nodeIDs is g's node ids in order.
func nodeIDs(g Graph) []string {
	ids := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		ids[i] = n.Record.ID
	}
	return ids
}

// node is g's node for id, failing the test when there is none.
func node(t *testing.T, g Graph, id string) Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Record.ID == id {
			return n
		}
	}
	t.Fatalf("graph has no node %s; nodes %v", id, nodeIDs(g))
	return Node{}
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The walk follows declared edges both directions for the hops it's given,
// across scope, and says how each Record was reached: by which edge, in
// which direction relative to the Record it came from, how many hops out.
// Seeds have no reached. Nodes are newest first by sequence.
func TestWalkFollowsDeclaredEdgesBothWaysForTheGivenHops(t *testing.T) {
	f := newFixture(t)
	other := f.startSession("proj-b")

	unrelated := f.record(store.KindNote, "no edges at all")
	root := f.record(store.KindDecision, "the seed")
	in1 := f.record(store.KindDecision, "supersedes the seed")
	out1 := f.record(store.KindNote, "informed by the seed")
	hop2 := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindOutcome, Text: "caused by in1, in another Project",
		SessionID: other, ProjectKey: "proj-b",
	})
	hop3 := f.record(store.KindNote, "three hops out")

	f.link(in1, root, store.EdgeSupersedes)
	f.link(root, out1, store.EdgeInforms)
	f.link(in1, hop2, store.EdgeCaused)
	f.link(hop3, hop2, store.EdgeProducedOutcome)

	g := f.walk(Limits{Hops: 2}, root)

	if want := []string{hop2, out1, in1, root}; !sameIDs(nodeIDs(g), want) {
		t.Fatalf("nodes = %v, want %v (unrelated %s and hop3 %s left out)", nodeIDs(g), want, unrelated, hop3)
	}
	if r := node(t, g, root).Reached; r != nil {
		t.Errorf("seed's reached = %+v, want nil", r)
	}
	wants := map[string]Reached{
		in1:  {Via: ViaEdge, Type: store.EdgeSupersedes, Direction: DirectionIn, From: root, Hops: 1},
		out1: {Via: ViaEdge, Type: store.EdgeInforms, Direction: DirectionOut, From: root, Hops: 1},
		hop2: {Via: ViaEdge, Type: store.EdgeCaused, Direction: DirectionOut, From: in1, Hops: 2},
	}
	for id, want := range wants {
		r := node(t, g, id).Reached
		if r == nil || *r != want {
			t.Errorf("reached of %s = %+v, want %+v", id, r, want)
		}
	}

	if one := f.walk(Limits{Hops: 1}, root); !sameIDs(nodeIDs(one), []string{out1, in1, root}) {
		t.Errorf("1-hop nodes = %v, want %v", nodeIDs(one), []string{out1, in1, root})
	}
}

// A Record reachable several ways is one node, reached by its fewest hops;
// a seed reachable from another seed is still a seed. Edges are the
// declared edges among the nodes, each once, and none to a Record outside.
func TestWalkKeepsEachRecordOnceByItsFewestHops(t *testing.T) {
	f := newFixture(t)
	seedA := f.record(store.KindDecision, "seed A")
	seedB := f.record(store.KindDecision, "seed B")
	near := f.record(store.KindNote, "one hop from A, two via mid")
	mid := f.record(store.KindNote, "between A and near")
	outside := f.record(store.KindNote, "two hops out")

	f.link(seedA, seedB, store.EdgeInforms)
	f.link(seedA, mid, store.EdgeCaused)
	f.link(mid, near, store.EdgeInforms)
	f.link(near, seedA, store.EdgeInforms)
	f.link(outside, mid, store.EdgeInforms)

	g := f.walk(Limits{Hops: 1}, seedA, seedB)

	if want := []string{mid, near, seedB, seedA}; !sameIDs(nodeIDs(g), want) {
		t.Fatalf("nodes = %v, want %v", nodeIDs(g), want)
	}
	if r := node(t, g, seedB).Reached; r != nil {
		t.Errorf("seed B's reached = %+v, want nil: it is a seed", r)
	}
	if r := node(t, g, near).Reached; r == nil || r.Hops != 1 || r.From != seedA || r.Direction != DirectionIn {
		t.Errorf("near's reached = %+v, want 1 hop, in, from seed A", r)
	}

	want := map[store.Edge]bool{
		{FromID: seedA, ToID: seedB, Type: store.EdgeInforms, DeclaredBy: f.session}: true,
		{FromID: seedA, ToID: mid, Type: store.EdgeCaused, DeclaredBy: f.session}:    true,
		{FromID: mid, ToID: near, Type: store.EdgeInforms, DeclaredBy: f.session}:    true,
		{FromID: near, ToID: seedA, Type: store.EdgeInforms, DeclaredBy: f.session}:  true,
	}
	if len(g.Edges) != len(want) {
		t.Fatalf("edges = %+v, want the %d among the nodes", g.Edges, len(want))
	}
	for _, e := range g.Edges {
		if !want[e] {
			t.Errorf("unexpected or repeated edge %+v", e)
		}
		delete(want, e)
	}
}

// Each node carries its status: Tombstoned first, then superseded, then
// Draft, else current. A Tombstoned node is kept, text-less, with its kind,
// Tier and ts, and the walk goes on through its edges.
func TestWalkNodesCarryTheirStatusAndTombstonedOnesAreTextless(t *testing.T) {
	f := newFixture(t)
	root := f.record(store.KindDecision, "the seed")
	newer := f.record(store.KindDecision, "supersedes the seed")
	draft := f.insert(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: "a guess"})
	forgotten := f.insert(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: "secret text", Next: "secret next",
	})
	beyond := f.record(store.KindNote, "reached only through the Tombstoned one")

	f.link(newer, root, store.EdgeSupersedes)
	f.link(root, draft, store.EdgeInforms)
	f.link(root, forgotten, store.EdgeInforms)
	f.link(forgotten, beyond, store.EdgeInforms)
	if err := f.st.TombstoneRecord(forgotten, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatalf("TombstoneRecord: %v", err)
	}

	g := f.walk(Limits{Hops: 2}, root)

	for id, want := range map[string]Status{
		root: StatusSuperseded, newer: StatusCurrent, draft: StatusDraft, forgotten: StatusTombstoned, beyond: StatusCurrent,
	} {
		if got := node(t, g, id).Status; got != want {
			t.Errorf("status of %s = %q, want %q", id, got, want)
		}
	}
	n := node(t, g, forgotten)
	if n.Record.Text != "" || n.Record.Next != "" {
		t.Errorf("Tombstoned node text %q next %q, want both empty", n.Record.Text, n.Record.Next)
	}
	if n.Record.Kind != store.KindHandoff || n.Record.Tier != store.TierAgentDeclared || n.Record.TS.IsZero() {
		t.Errorf("Tombstoned node = %+v, want its kind, Tier and ts kept", n.Record)
	}
	if r := node(t, g, beyond).Reached; r == nil || r.From != forgotten || r.Hops != 2 {
		t.Errorf("beyond's reached = %+v, want 2 hops from the Tombstoned node", r)
	}
}

// confirm writes p in the fixture's Project, failing the test on an error.
func (f *fixture) confirm(p store.ConfirmParams) string {
	f.t.Helper()
	if p.ProjectKey == "" {
		p.ProjectKey = f.project
	}
	id, err := f.st.Confirm(p)
	if err != nil {
		f.t.Fatalf("Confirm %s: %v", p.Action, err)
	}
	return id
}

// Done, Affirm and Promote confirm Records an edge reaches are not nodes,
// and the walk doesn't go through them; their target carries them as
// confirmed_by, with the type the store wrote. A Contradiction is a confirm
// Record too, but it stays a node with its real contradicts edge.
func TestWalkFoldsConfirmRecordsIntoTheirTarget(t *testing.T) {
	f := newFixture(t)
	promoter := f.startSession(f.project)
	h := f.record(store.KindHandoff, "the seed")
	draft := f.insert(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: "a guess"})
	f.link(h, draft, store.EdgeInforms)

	affirm := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmAffirm, RecordID: h, SessionID: f.session})
	done := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityHuman, Actor: "human"}, Action: store.ConfirmDone, RecordID: h})
	promote := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmPromote, RecordID: draft, SessionID: promoter})
	behind := f.record(store.KindNote, "linked only to the affirm")
	f.link(behind, affirm, store.EdgeInforms)
	contradiction := f.contradict(h)

	g := f.walk(Limits{Hops: 2}, h)

	if want := []string{contradiction, draft, h}; !sameIDs(nodeIDs(g), want) {
		t.Fatalf("nodes = %v, want %v (no affirm %s, done %s, promote %s, nor behind %s)", nodeIDs(g), want, affirm, done, promote, behind)
	}
	for _, e := range g.Edges {
		if e.Type == store.EdgeInforms && e.ToID == h {
			t.Errorf("edge %+v listed; a confirm's informs edge isn't", e)
		}
	}
	if r := node(t, g, contradiction).Reached; r == nil || r.Type != store.EdgeContradicts || r.Direction != DirectionIn {
		t.Errorf("contradiction's reached = %+v, want in along contradicts", r)
	}

	type want struct {
		id  string
		typ store.ConfirmAction
	}
	check := func(target string, wants ...want) {
		t.Helper()
		got := node(t, g, target).ConfirmedBy
		if len(got) != len(wants) {
			t.Fatalf("confirmed_by of %s = %+v, want %d", target, got, len(wants))
		}
		for i, w := range wants {
			if got[i].ID != w.id || got[i].Type != w.typ {
				t.Errorf("confirmed_by[%d] of %s = %+v, want %s by %s", i, target, got[i], w.typ, w.id)
			}
		}
	}
	check(h, want{affirm, store.ConfirmAffirm}, want{done, store.ConfirmDone})
	check(draft, want{promote, store.ConfirmPromote})
	if got := node(t, g, draft).Status; got != StatusDraft {
		t.Errorf("promoted Draft's status = %q, want %q", got, StatusDraft)
	}
	if got := node(t, g, contradiction).ConfirmedBy; got == nil || len(got) != 0 {
		t.Errorf("unconfirmed node's confirmed_by = %#v, want empty, not nil", got)
	}

}

// A seed is kept even when it is a Done, Affirm or Promote confirm Record,
// since its caller named it, and the walk goes on from it. What it reaches
// follows the usual rules: its target is a node, and the target's other
// confirms fold into it.
func TestWalkKeepsAConfirmSeed(t *testing.T) {
	f := newFixture(t)
	h := f.record(store.KindHandoff, "the target")
	affirm := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmAffirm, RecordID: h, SessionID: f.session})
	done := f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityHuman, Actor: "human"}, Action: store.ConfirmDone, RecordID: h})
	behind := f.record(store.KindNote, "linked to the affirm")
	f.link(behind, affirm, store.EdgeInforms)

	g := f.walk(Limits{Hops: 2}, affirm)

	if want := []string{behind, affirm, h}; !sameIDs(nodeIDs(g), want) {
		t.Fatalf("nodes = %v, want %v (not done %s, folded into its target)", nodeIDs(g), want, done)
	}
	if r := node(t, g, affirm).Reached; r != nil {
		t.Errorf("affirm's reached = %+v, want nil: it is the seed", r)
	}
	if r := node(t, g, h).Reached; r == nil || r.From != affirm || r.Type != store.EdgeInforms || r.Direction != DirectionOut || r.Hops != 1 {
		t.Errorf("target's reached = %+v, want out from the affirm along informs, 1 hop", r)
	}
	if got := node(t, g, h).ConfirmedBy; len(got) != 2 || got[0].ID != affirm || got[1].ID != done {
		t.Errorf("target's confirmed_by = %+v, want the affirm then the done", got)
	}
}

// contradict files a Contradiction of target citing a fresh tool event.
func (f *fixture) contradict(target string) string {
	f.t.Helper()
	ev, err := f.st.AppendEvent(store.Event{TS: time.Now(), Kind: "tool.use", SessionID: f.session, Source: "shell", Payload: `{"name":"Bash"}`})
	if err != nil {
		f.t.Fatalf("AppendEvent: %v", err)
	}
	return f.confirm(store.ConfirmParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Action: store.ConfirmContradict, RecordID: target,
		Evidence: []int64{ev}, SessionID: f.session,
	})
}

// A Handoff node carries handoff's status for it: Done, or Possibly stale
// with its Evidence. Other kinds, and a Tombstoned Handoff, carry none.
func TestWalkHandoffNodesCarryTheirHandoffStatus(t *testing.T) {
	f := newFixture(t)
	root := f.record(store.KindDecision, "the seed")
	finished := f.record(store.KindHandoff, "finished work")
	stale := f.record(store.KindHandoff, "work since contradicted")
	forgotten := f.record(store.KindHandoff, "to be forgotten")
	for _, h := range []string{finished, stale, forgotten} {
		f.link(h, root, store.EdgeInforms)
	}
	f.confirm(store.ConfirmParams{Identity: store.Identity{Kind: store.IdentityHuman, Actor: "human"}, Action: store.ConfirmDone, RecordID: finished})
	contradiction := f.contradict(stale)
	if err := f.st.TombstoneRecord(forgotten, store.Identity{Kind: store.IdentityHuman}); err != nil {
		t.Fatalf("TombstoneRecord: %v", err)
	}

	g := f.walk(Limits{Hops: 1}, root)

	if hs := node(t, g, finished).Handoff; hs == nil || !hs.Done || hs.PossiblyStale() {
		t.Errorf("done Handoff's status = %+v, want done, not possibly stale", hs)
	}
	hs := node(t, g, stale).Handoff
	if hs == nil || hs.Done || !hs.PossiblyStale() {
		t.Fatalf("contradicted Handoff's status = %+v, want possibly stale", hs)
	}
	if ev := hs.Evidence(); len(ev) != 1 || ev[0] != contradiction {
		t.Errorf("possibly stale Handoff's evidence = %v, want [%s]", ev, contradiction)
	}
	for _, id := range []string{root, forgotten} {
		if hs := node(t, g, id).Handoff; hs != nil {
			t.Errorf("handoff status of %s = %+v, want nil", id, hs)
		}
	}
}

// An expired Draft reached by an edge is left out, and the walk doesn't go
// through it (#32: "Expired Drafts are left out"). A live Draft and an
// expired Record that isn't a Draft stay, and so does an expired Draft
// that is itself a seed.
func TestWalkLeavesOutReachedExpiredDrafts(t *testing.T) {
	f := newFixture(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	draft := func(text string, expires time.Time) string {
		return f.insert(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityInference}, Kind: store.KindNote, Text: text, ExpiresAt: &expires})
	}
	root := f.record(store.KindDecision, "the seed")
	expired := draft("an expired guess", past)
	beyond := f.record(store.KindNote, "reached only through the expired Draft")
	live := draft("a live guess", future)
	lapsed := f.insert(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindNote, Text: "expired, but declared", ExpiresAt: &past})
	f.link(root, expired, store.EdgeInforms)
	f.link(expired, beyond, store.EdgeInforms)
	f.link(root, live, store.EdgeInforms)
	f.link(root, lapsed, store.EdgeInforms)

	if got, want := nodeIDs(f.walk(Limits{Hops: 2}, root)), []string{lapsed, live, root}; !sameIDs(got, want) {
		t.Errorf("nodes = %v, want %v (no expired Draft %s, nothing through it)", got, want, expired)
	}
	if got, want := nodeIDs(f.walk(Limits{Hops: 1}, expired)), []string{beyond, expired, root}; !sameIDs(got, want) {
		t.Errorf("nodes seeded on the expired Draft = %v, want %v", got, want)
	}
}
