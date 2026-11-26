package handoff_test

import (
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/handoff"
	"github.com/RidgetopAi/backstory/internal/store"
)

func exit(n int) *int { return &n }

// The short label is one clause per reason, in reason order, joined by
// "; ": what the SessionStart block's marker, recall's label and This
// Week's summary all show. It cites no ids; Evidence does that.
func TestLabelWordsEveryReasonShortly(t *testing.T) {
	long := "go test -run TestSomethingVeryLongIndeed ./internal/handoff/... -count=1 -v"
	s := handoff.Status{Reasons: []store.FreshnessReason{
		{Kind: store.FreshnessContradicted, RecordIDs: []string{"r1"}},
		{Kind: store.FreshnessLaterRecord, RecordIDs: []string{"r2", "r3"}},
		{Kind: store.FreshnessLaterActivity, EventIDs: []int64{7}},
		{Kind: store.FreshnessLaterFailure, EventIDs: []int64{8, 9}, Failures: []store.FailureDetail{
			{EventID: 8, Tool: "Bash", Command: "make lint", Exit: exit(2)},
			{EventID: 9, Tool: "Bash", Command: long, Exit: exit(1)},
		}},
		{Kind: store.FreshnessLaterUnrecordedWork, EventIDs: []int64{10}, Sessions: 1, Files: 2},
	}}
	want := strings.Join([]string{
		"contradicted by a later record",
		"2 later records share a path it names",
		"a later edit to a file it names",
		"go test -run TestSomethingVeryLongIndeed ./internal/handoff/…" + " failed (exit 1) after it (+1 more)",
		"1 later session edited 2 files and wrote no note",
	}, "; ")
	if got := s.Label(); got != want {
		t.Fatalf("Label =\n  %q\nwant\n  %q", got, want)
	}
}

// Counts read in the plural past one, and a failure with no recorded
// command or exit still says what failed.
func TestLabelPluralsAndBareFailure(t *testing.T) {
	s := handoff.Status{Reasons: []store.FreshnessReason{
		{Kind: store.FreshnessContradicted, RecordIDs: []string{"r1", "r2"}},
		{Kind: store.FreshnessLaterRecord, RecordIDs: []string{"r3"}},
		{Kind: store.FreshnessLaterActivity, EventIDs: []int64{4, 5, 6}},
		{Kind: store.FreshnessLaterFailure, EventIDs: []int64{7}, Failures: []store.FailureDetail{{EventID: 7}}},
		{Kind: store.FreshnessLaterUnrecordedWork, EventIDs: []int64{8, 9}, Sessions: 2, Files: 1},
	}}
	want := "contradicted by 2 later records; a later record shares a path it names; " +
		"3 later edits to files it names; a tool call failed after it; " +
		"2 later sessions edited 1 file and wrote no note"
	if got := s.Label(); got != want {
		t.Fatalf("Label =\n  %q\nwant\n  %q", got, want)
	}
	if got := (handoff.Status{}).Label(); got != "" {
		t.Fatalf("a fresh Handoff's Label = %q, want empty", got)
	}
}

// record writes a Record of kind with text in the fixture's project.
func (f *fixture) record(kind store.RecordKind, text string) string {
	f.t.Helper()
	id, err := f.st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: kind, Text: text,
		About: []string{"main.go"}, SessionID: f.session, ProjectKey: projectKey,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func line(t *testing.T, st *store.Store, r store.FreshnessReason) string {
	t.Helper()
	got, err := handoff.Line(st, r)
	if err != nil {
		t.Fatalf("handoff.Line(%s): %v", r.Kind, err)
	}
	return got
}

// The long line is one reason in full, for This Week's detailed reasons:
// the newest cited Record's first line, a later Outcome named first (it is
// what makes the Handoff look finished), the failed command untruncated.
// Several cited records or events describe the newest and count the rest.
func TestLineWordsOneReasonInFull(t *testing.T) {
	f := newFixture(t)
	older := f.record(store.KindNote, "main.go now builds with -race\nmore detail")
	newer := f.record(store.KindDecision, "Switched to a different approach")
	outcome := f.record(store.KindOutcome, "Shipped the CLI\nall green")
	long := "go test -run TestSomethingVeryLongIndeed ./internal/handoff/... -count=1 -v"
	for _, c := range []struct {
		r    store.FreshnessReason
		want string
	}{
		{store.FreshnessReason{Kind: store.FreshnessContradicted, RecordIDs: []string{older}},
			"contradicted by a later record: main.go now builds with -race"},
		{store.FreshnessReason{Kind: store.FreshnessLaterRecord, RecordIDs: []string{older, newer}},
			"Switched to a different approach (+1 more)"},
		{store.FreshnessReason{Kind: store.FreshnessLaterRecord, RecordIDs: []string{outcome, older, newer}},
			"outcome " + outcome[:8] + ": Shipped the CLI (+2 more)"},
		{store.FreshnessReason{Kind: store.FreshnessLaterActivity, EventIDs: []int64{4, 5}},
			"2 later edits to files the handoff names"},
		{store.FreshnessReason{Kind: store.FreshnessLaterActivity, EventIDs: []int64{4}},
			"a later edit to a file the handoff names"},
		{store.FreshnessReason{Kind: store.FreshnessLaterFailure, EventIDs: []int64{8, 9}, Failures: []store.FailureDetail{
			{EventID: 8, Tool: "Bash", Command: "make lint", Exit: exit(2)},
			{EventID: 9, Tool: "Bash", Command: long, Exit: exit(1)},
		}}, long + " failed (exit 1) (+1 more)"},
		{store.FreshnessReason{Kind: store.FreshnessLaterFailure, EventIDs: []int64{8}, Failures: []store.FailureDetail{
			{EventID: 8, Tool: "Edit"},
		}}, "Edit failed"},
		{store.FreshnessReason{Kind: store.FreshnessLaterUnrecordedWork, EventIDs: []int64{10}, Sessions: 1, Files: 2},
			"1 session edited 2 files and wrote no note"},
	} {
		if got := line(t, f.st, c.r); got != c.want {
			t.Errorf("Line(%s %v) =\n  %q\nwant\n  %q", c.r.Kind, c.r.RecordIDs, got, c.want)
		}
	}
}
