package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

// runEditStdin runs `backstory edit --stdin id` in-process with text on
// stdin and returns the replacement's id.
func runEditStdin(t *testing.T, id, text string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run([]string{"edit", "--stdin", id}, strings.NewReader(text), &out, &errb); code != 0 {
		t.Fatalf("edit %s exit %d: %s%s", id, code, out.String(), errb.String())
	}
	_, newID, ok := strings.Cut(strings.TrimSpace(out.String()), " → ")
	if !ok {
		t.Fatalf("edit stdout = %q, want \"edited <old> → <new>\"", out.String())
	}
	return newID
}

// renderBlockAt is the SessionStart block a Session starting at dir reads.
func renderBlockAt(t *testing.T, dbPath, dir string, workspaces []string) string {
	t.Helper()
	st, err := store.Open(dbPath, workspaces, project.RealGit{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	out, err := block.Render(block.Params{
		Store: st, ProjectKey: project.Key(dir, project.RealGit{}, workspaces), SessionID: "caller",
		CWD: dir, Git: project.RealGit{}, WorkspaceDirs: workspaces, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// thisWeekHandoffAt is the handoff_id of This Week's row for folder dir.
func thisWeekHandoffAt(t *testing.T, dir string) (string, bool) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run([]string{"this-week", "--json"}, bytes.NewReader(nil), &out, &errb); code != 0 {
		t.Fatalf("this-week exit %d: %s", code, errb.String())
	}
	var o thisWeekOutputJSON
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("bad this-week JSON %q: %v", out.String(), err)
	}
	for _, r := range o.WhereLeftOff {
		if r.Project != nil && filepath.Clean(r.Project.CWD) == filepath.Clean(dir) {
			return r.Project.HandoffID, true
		}
	}
	return "", false
}

// An edit's replacement stays where its original was found (#51): a Handoff
// filed under the Workspace's key, written at the Workspace root, in a
// non-git folder directly under it, or in one deeper down, is replaced at
// that same location. The SessionStart block there resumes the replacement
// and This Week's row for that folder lists it, not the superseded original.
func TestEditKeepsAWorkspaceHandoffAtItsLocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{"workspace root", ""},
		{"non-git folder under it", "notes"},
		{"non-git folder deeper under it", filepath.Join("group", "notes")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			ws := filepath.Join(home, "projects")
			dir := filepath.Join(ws, tc.rel)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			dbPath := filepath.Join(dataDir, "backstory", "backstory.db")
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", dataDir)
			t.Setenv("BACKSTORY_WORKSPACE_DIRS", ws)
			// Keep the fixture's own (temp-dir) HOME out of This Week's excluded roots.
			t.Setenv("TMPDIR", "/nonexistent-backstory-tmp")
			t.Setenv("XDG_RUNTIME_DIR", "")
			workspaces := []string{ws}
			wsKey := "workspace:" + ws

			st, err := store.Open(dbPath, workspaces, project.RealGit{})
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-time.Hour)
			if err := st.UpsertProject(store.Project{Key: wsKey, Toplevel: ws, FirstSeen: at}); err != nil {
				t.Fatal(err)
			}
			ownKey := project.Key(dir, project.RealGit{}, workspaces)
			if ownKey != wsKey {
				if err := st.UpsertProject(store.Project{Key: ownKey, Toplevel: dir, FirstSeen: at}); err != nil {
					t.Fatal(err)
				}
			}
			sid, err := st.StartSession(store.StartSessionParams{
				Agent: "claude", CWD: dir, ProjectKey: ownKey, StartedAt: at, Origin: store.OriginLive,
			})
			if err != nil {
				t.Fatal(err)
			}
			p, _ := json.Marshal(map[string]string{"name": "Write", "path": filepath.Join(dir, "notes.md")})
			mustAppendEvent(t, st, store.Event{TS: at.Add(time.Minute), Kind: "tool.use", SessionID: sid, Source: "hook", Payload: string(p)})
			filedAt := project.HandoffHome(dir, ownKey, workspaces)
			if filedAt != wsKey {
				t.Fatalf("fixture: HandoffHome(%s) = %q, want the Workspace's %q", dir, filedAt, wsKey)
			}
			origID, err := st.InsertRecord(store.InsertRecordParams{
				Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff,
				Text: "ORIGINAL handoff", SessionID: sid, ProjectKey: filedAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if got, ok := thisWeekHandoffAt(t, dir); !ok || got != origID {
				t.Fatalf("fixture: This Week's row at %s = %q (found %v), want the original %s", dir, got, ok, origID)
			}

			newID := runEditStdin(t, origID, "EDITED handoff\n")

			blk := renderBlockAt(t, dbPath, dir, workspaces)
			if !strings.Contains(blk, "Resume: (id "+newID+")") || !strings.Contains(blk, "EDITED handoff") {
				t.Errorf("block at %s = %q, want Resume of the replacement %s", dir, blk, newID)
			}
			if strings.Contains(blk, "ORIGINAL handoff") {
				t.Errorf("block at %s still shows the superseded original:\n%s", dir, blk)
			}
			if got, ok := thisWeekHandoffAt(t, dir); !ok || got != newID {
				t.Errorf("This Week's row at %s lists handoff %q (found %v), want the replacement %s", dir, got, ok, newID)
			}
		})
	}
}
