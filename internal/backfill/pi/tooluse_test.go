package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/payload"
)

// TestBackfilledToolUsesMatchLiveCapture: a backfilled Pi session records
// the same tool.use payloads Pi's live hook records for the same calls.
// The session file keeps Pi's own tool names and arguments (edit with a
// path, bash with a command); the extension renames them before the hook
// sees a call, so a backfilled edit must still count as a changed file.
func TestBackfilledToolUsesMatchLiveCapture(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--home-bob-shape--")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	fixture := strings.Join([]string{
		`{"id":"root-s","parentId":"","timestamp":"2026-05-03T00:00:00Z","type":"session","cwd":"/home/bob/shape"}`,
		`{"id":"user-s1","parentId":"root-s","timestamp":"2026-05-03T00:00:01Z","type":"message","role":"user","text":"fix it"}`,
		`{"id":"asst-s1","parentId":"user-s1","timestamp":"2026-05-03T00:00:02Z","type":"message","role":"assistant","content":[` +
			`{"type":"toolCall","id":"call-edit","name":"edit","arguments":{"path":"main.go","oldText":"x","newText":"y"}},` +
			`{"type":"toolCall","id":"call-bash","name":"bash","arguments":{"command":"go test ./..."}},` +
			`{"type":"toolCall","id":"call-grep","name":"grep","arguments":{"pattern":"TODO"}}]}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "2026-05-03T00-00-00Z_root-s.jsonl"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	st := mustOpenStore(t)
	if _, err := Import(st, Options{Root: root, Git: fakeGit{}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	rows, err := st.DB().Query(`SELECT payload FROM timeline_events WHERE kind = ? ORDER BY id`, EventToolUse)
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
		{ToolUseID: "call-edit", Name: "Edit", Path: "main.go"},
		{ToolUseID: "call-bash", Name: "Bash", Command: "go test ./..."},
		{ToolUseID: "call-grep", Name: "grep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backfilled tool.use payloads =\n  %+v\nwant\n  %+v", got, want)
	}
}
