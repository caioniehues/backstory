package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// queueIDs is the ids EmbedQueue returns for model, in order.
func queueIDs(t *testing.T, s *Store, model string, limit int, exclude []string) []string {
	t.Helper()
	q, err := s.EmbedQueue(model, limit, exclude)
	if err != nil {
		t.Fatalf("EmbedQueue: %v", err)
	}
	ids := make([]string, len(q))
	for i, r := range q {
		ids[i] = r.RecordID
	}
	return ids
}

// The queue is every Record of an embedded kind that isn't Tombstoned and
// has no vector for the fingerprint, newest first: superseded Records,
// Drafts and Records from Peers included; Claims and other fingerprints'
// vectors don't count.
func TestEmbedQueueIsTheUnembeddedRecordsOfEmbeddedKinds(t *testing.T) {
	a := mustOpen(t, filepath.Join(t.TempDir(), "a.db"))
	s := mustOpen(t, filepath.Join(t.TempDir(), "b.db"))
	seedRemoteProject(t, a, "/a/widgets", "https://github.com/acme/widgets")
	seedRemoteProject(t, s, "/b/widgets", "git@github.com:acme/widgets.git")
	peer := insertBranchHandoff(t, a, "/a/widgets", "main")
	changes, _, _, err := a.ChangesSince(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSyncChanges(Identity{Kind: IdentityHuman, Actor: humanActor}, SyncPeer{DeviceID: "dev-a"}, changes); err != nil {
		t.Fatal(err)
	}

	agent := Identity{Kind: IdentityAgent}
	insert := func(p InsertRecordParams, edges ...EdgeSpec) string {
		t.Helper()
		id, err := s.InsertRecordWithEdges(p, edges)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := insert(InsertRecordParams{Identity: agent, Kind: KindDecision, Text: "old decision"})
	newer := insert(InsertRecordParams{Identity: agent, Kind: KindDecision, Text: "new decision"},
		EdgeSpec{OtherID: old, Type: EdgeSupersedes, DeclaredBy: "test"})
	yes := OutcomeTrue
	outcome := insert(InsertRecordParams{Identity: agent, Kind: KindOutcome, Text: "it worked", Outcome: &yes})
	expired := time.Now().Add(-time.Hour)
	draft := insert(InsertRecordParams{Identity: Identity{Kind: IdentityInference}, Kind: KindNote, Text: "a draft", ExpiresAt: &expired})
	insert(InsertRecordParams{Identity: agent, Kind: KindClaim, Text: "a claim"})
	gone := insert(InsertRecordParams{Identity: agent, Kind: KindNote, Text: "forgotten"})
	if err := s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	embedded := insert(InsertRecordParams{Identity: agent, Kind: KindNote, Text: "already embedded"})
	mustUpsertVector(t, s, embedded, testModel, []float32{1, 2, 3})
	onOtherModel := insert(InsertRecordParams{Identity: agent, Kind: KindHandoff, Text: "other model"})
	mustUpsertVector(t, s, onOtherModel, "older-model|3", []float32{1, 2, 3})

	want := []string{onOtherModel, draft, outcome, newer, old, peer}
	if got := queueIDs(t, s, testModel, 100, nil); !slices.Equal(got, want) {
		t.Fatalf("queue = %v\nwant    %v (newest first)", got, want)
	}
	if got := queueIDs(t, s, testModel, 2, nil); !slices.Equal(got, want[:2]) {
		t.Fatalf("queue limit 2 = %v, want %v", got, want[:2])
	}
	if got := queueIDs(t, s, testModel, 100, []string{draft, old}); !slices.Equal(got, []string{onOtherModel, outcome, newer, peer}) {
		t.Fatalf("queue excluding two = %v", got)
	}

	counts, err := s.EmbedCounts(testModel)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (EmbedCounts{Embedded: 1, Queued: 6}) {
		t.Fatalf("counts = %+v, want 1 embedded, 6 queued", counts)
	}
}

// The document is the text, plus a blank line and next for a Handoff.
func TestEmbedQueueDocumentAddsAHandoffsNext(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	agent := Identity{Kind: IdentityAgent}
	ids := map[string]string{}
	for name, p := range map[string]InsertRecordParams{
		"handoff":      {Identity: agent, Kind: KindHandoff, Text: "stopped mid-way\nline two", Next: "run the migration"},
		"handoff-bare": {Identity: agent, Kind: KindHandoff, Text: "nothing next"},
		"decision":     {Identity: agent, Kind: KindDecision, Text: "use sqlite"},
	} {
		id, err := s.InsertRecord(p)
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = name
	}
	q, err := s.EmbedQueue(testModel, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"handoff":      "stopped mid-way\nline two\n\nrun the migration",
		"handoff-bare": "nothing next",
		"decision":     "use sqlite",
	}
	if len(q) != 3 {
		t.Fatalf("queue has %d Records, want 3", len(q))
	}
	for _, r := range q {
		if name := ids[r.RecordID]; r.Document != want[name] {
			t.Errorf("%s document = %q, want %q", name, r.Document, want[name])
		}
	}
}
