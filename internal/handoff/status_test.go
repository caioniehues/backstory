package handoff_test

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/handoff"
	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/store"
)

const projectKey = "/proj/.git"

// fixture is one project with one agent session that wrote a Handoff.
type fixture struct {
	t       *testing.T
	st      *store.Store
	session string
	h       store.Record
	ts      time.Time
	use     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertProject(store.Project{Key: projectKey, Toplevel: "/proj", FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, ts: time.Now()}
	f.session = f.startSession("claude")
	id, err := st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: "resume at main.go",
		About: []string{"main.go"}, SessionID: f.session, ProjectKey: projectKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.h, err = st.GetRecord(id); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) startSession(agent string) string {
	f.t.Helper()
	id, err := f.st.StartSession(store.StartSessionParams{Agent: agent, CWD: "/proj", ProjectKey: projectKey, StartedAt: time.Now(), Origin: store.OriginLive})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) event(session, kind string, pl any) int64 {
	f.t.Helper()
	b, err := json.Marshal(pl)
	if err != nil {
		f.t.Fatal(err)
	}
	f.ts = f.ts.Add(time.Second)
	id, err := f.st.AppendEvent(store.Event{TS: f.ts, Kind: kind, SessionID: session, Source: "posttooluse", Payload: string(b)})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// run appends the agent's Bash tool.use of cmd and its result, returning the
// result's event id. exit 0 is a pass.
func (f *fixture) run(cmd string, exit int) int64 {
	f.t.Helper()
	f.use++
	useID := "tu" + strconv.Itoa(f.use)
	f.event(f.session, payload.KindToolUse, payload.ToolUse{ToolUseID: useID, Name: "Bash", Command: cmd})
	return f.event(f.session, payload.KindToolResult, payload.ToolResult{ToolUseID: useID, IsError: exit != 0, Exit: &exit})
}

func (f *fixture) status() handoff.Status {
	f.t.Helper()
	s, err := handoff.Of(f.st, f.h, nil)
	if err != nil {
		f.t.Fatalf("handoff.Of: %v", err)
	}
	return s
}

func failureEventIDs(s handoff.Status) []int64 {
	for _, r := range s.Reasons {
		if r.Kind == store.FreshnessLaterFailure {
			return r.EventIDs
		}
	}
	return nil
}

// A failure a later run of the same command passed is no longer Possibly
// stale evidence. Before this module only the block applied that rule; week
// and recall still flagged the handoff.
func TestClearedFailureIsNotPossiblyStale(t *testing.T) {
	f := newFixture(t)
	fail := f.run("go test ./...", 1)
	if got := failureEventIDs(f.status()); len(got) != 1 || got[0] != fail {
		t.Fatalf("open failure: later-failure evidence = %v, want [%d]", got, fail)
	}
	f.run("go test -v ./... 2>&1", 0)
	if s := f.status(); s.PossiblyStale() {
		t.Fatalf("cleared failure still Possibly stale: %+v", s.Reasons)
	}
}

// Only the cleared failure stops counting: one still open keeps the Handoff
// Possibly stale and is the only failure cited. The block used to keep every
// later failure, cleared ones included, while any one was still open.
func TestOnlyTheOpenFailureIsEvidence(t *testing.T) {
	f := newFixture(t)
	f.run("go test ./...", 1)
	stillOpen := f.run("make lint", 2)
	f.run("go test ./...", 0)
	s := f.status()
	if got := failureEventIDs(s); len(got) != 1 || got[0] != stillOpen {
		t.Fatalf("later-failure evidence = %v, want only the open failure [%d]", got, stillOpen)
	}
	for _, r := range s.Reasons {
		if r.Kind == store.FreshnessLaterFailure && (len(r.Failures) != 1 || r.Failures[0].Command != "make lint") {
			t.Fatalf("failure detail = %+v, want only `make lint`", r.Failures)
		}
	}
}

// The Human running the same command in their shell, in the handoff's
// folder, clears the agent's failure too; a pass whose exit is not the test
// runner's own (piped into tail) clears nothing.
func TestShellPassClearsButAPipedPassDoesNot(t *testing.T) {
	f := newFixture(t)
	f.run("go test ./...", 1)
	shell := f.startSession("shell")
	f.event(shell, payload.KindShellCommand, payload.ShellCommand{Cmd: "go test ./... | tail -3", CWD: "/proj", Exit: 0})
	if !f.status().PossiblyStale() {
		t.Fatal("a piped pass cleared the failure")
	}
	f.event(shell, payload.KindShellCommand, payload.ShellCommand{Cmd: "go test ./...", CWD: "/proj", Exit: 0})
	if s := f.status(); s.PossiblyStale() {
		t.Fatalf("the Human's shell pass did not clear the failure: %+v", s.Reasons)
	}
}

// Evidence is every reason's cited ids in reason order, record ids before
// event ids; never nil, so --json renders [] for a fresh Handoff.
func TestEvidenceListsEachReasonsIDsInOrder(t *testing.T) {
	f := newFixture(t)
	if got := f.status().Evidence(); got == nil || len(got) != 0 {
		t.Fatalf("fresh Handoff Evidence = %#v, want an empty non-nil list", got)
	}
	fail := f.run("go test ./...", 1)
	note, err := f.st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindNote, Text: "main.go moved",
		About: []string{"main.go"}, SessionID: f.session, ProjectKey: projectKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.status().Evidence()
	if want := []string{note, strconv.FormatInt(fail, 10)}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Evidence = %v, want %v (the later record, then the failure)", got, want)
	}
}

// A Done Handoff is never Possibly stale, whatever evidence follows it.
func TestDoneIsNeverPossiblyStale(t *testing.T) {
	f := newFixture(t)
	f.run("go test ./...", 1)
	if _, err := f.st.Confirm(store.ConfirmParams{
		Identity: store.Identity{Kind: store.IdentityHuman, Actor: "human"}, Action: store.ConfirmDone, RecordID: f.h.ID, ProjectKey: projectKey,
	}); err != nil {
		t.Fatal(err)
	}
	s := f.status()
	if !s.Done || s.DoneAt.IsZero() || s.PossiblyStale() {
		t.Fatalf("status = %+v, want Done with a time and not Possibly stale", s)
	}
}
