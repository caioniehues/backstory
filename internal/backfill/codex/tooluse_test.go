package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/payload"
)

// TestBackfilledToolUsesMatchLiveCapture: a backfilled rollout records the
// same tool.use payloads Codex's live hook records for the same calls. An
// apply_patch is one tool.use per touched path (so the Session counts its
// changed files), and shell and exec_command are both Bash with a command.
func TestBackfilledToolUsesMatchLiveCapture(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n*** Add File: b.go\n+package b\n*** End Patch"
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "02", "01")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	fixture := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-02-01T09:00:00Z","payload":{"id":"shape-thread","cwd":"/home/erin/shape","cli_version":"0.151.0"}}`,
		`{"type":"response_item","timestamp":"2026-02-01T09:00:01Z","payload":{"type":"custom_tool_call","name":"apply_patch","input":` + string(patchJSON) + `,"call_id":"call_patch"}}`,
		`{"type":"response_item","timestamp":"2026-02-01T09:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"call_patch","output":"Success."}}`,
		`{"type":"response_item","timestamp":"2026-02-01T09:00:03Z","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\",\"-la\"]}","call_id":"call_shell"}}`,
		`{"type":"response_item","timestamp":"2026-02-01T09:00:04Z","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}","call_id":"call_exec"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "rollout-20260201T090000-shape0thread.jsonl"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	st := mustOpenStore(t)
	if _, err := Import(st, Options{Root: root, Git: fakeGit{}, Workspaces: []string{}}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	sess := sessionByHarnessID(t, st, "shape-thread")
	rows, err := st.DB().Query(`SELECT payload FROM timeline_events WHERE session_id = ? AND kind = ? ORDER BY id`, sess.ID, EventToolUse)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []payload.ToolUse
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		var tu payload.ToolUse
		if err := json.Unmarshal([]byte(p), &tu); err != nil {
			t.Fatal(err)
		}
		got = append(got, tu)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []payload.ToolUse{
		{ToolUseID: "call_patch", Name: "apply_patch", Path: "a.go"},
		{ToolUseID: "call_patch", Name: "apply_patch", Path: "b.go"},
		{ToolUseID: "call_shell", Name: "Bash", Command: "ls -la"},
		{ToolUseID: "call_exec", Name: "Bash", Command: "go test ./..."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backfilled tool.use payloads =\n  %+v\nwant\n  %+v", got, want)
	}
}
