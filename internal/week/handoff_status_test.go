package week

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/store"
)

// A failure a later run of the same command passed is not Possibly stale
// evidence on This Week either: the block already said so, This Week still
// listed the handoff under Needs you (#2).
func TestClearedFailureIsNotAPossiblyStaleHandoff(t *testing.T) {
	st := openTestStore(t)
	const key = "proj-cleared"
	upsertProject(t, st, key, "/home/brian/cleared")
	start := fixtureNow.Add(-2 * 24 * time.Hour)
	sid := startSession(t, st, "sess-cleared", key, "/home/brian/cleared", start)
	appendEvent(t, st, store.Event{TS: start, Kind: "session.start", SessionID: sid, Source: "shell", Payload: `{}`})
	if _, err := st.InsertRecord(store.InsertRecordParams{
		Identity: agentIdentity, Kind: store.KindHandoff, Text: "tests pass", SessionID: sid, ProjectKey: key,
	}); err != nil {
		t.Fatal(err)
	}
	ts := start
	run := func(useID string, exit int) {
		t.Helper()
		ts = ts.Add(time.Minute)
		use, _ := json.Marshal(payload.ToolUse{ToolUseID: useID, Name: "Bash", Command: "go test ./..."})
		appendEvent(t, st, store.Event{TS: ts, Kind: payload.KindToolUse, SessionID: sid, Source: "posttooluse", Payload: string(use)})
		res, _ := json.Marshal(payload.ToolResult{ToolUseID: useID, IsError: exit != 0, Exit: &exit})
		appendEvent(t, st, store.Event{TS: ts, Kind: payload.KindToolResult, SessionID: sid, Source: "posttooluse", Payload: string(res)})
	}
	build := func() (ProjectSummary, bool) {
		t.Helper()
		res, err := Build(Params{Store: st, Now: fixtureNow})
		if err != nil {
			t.Fatal(err)
		}
		_, stale := staleItemFor(res, key)
		return rowByKey(t, res, key), stale
	}

	run("t1", 1)
	if row, stale := build(); !stale || !row.HandoffStale {
		t.Fatalf("open failure: stale item %v, HandoffStale %v; want both", stale, row.HandoffStale)
	}
	run("t2", 0)
	if row, stale := build(); stale || row.HandoffStale {
		t.Fatalf("cleared failure: stale item %v, HandoffStale %v; want neither", stale, row.HandoffStale)
	}
}
