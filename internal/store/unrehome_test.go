package store

import (
	"path/filepath"
	"testing"
)

// The repair at every Open (#5): the pre-#5 re-home moved a Handoff written
// in a non-git folder deeper under a Workspace onto the folder's own path, as
// if it were a repo. Each Open moves such a Handoff back to the Workspace's
// key, where project.HandoffHome files it under the Workspaces configured at
// that Open, after copying the store beside itself (forgetting.Unrehome), and
// queues it for Peers. A repo's Handoff, a Handoff of a folder outside every
// Workspace and a non-Handoff stay where they are. A second Open moves
// nothing and writes no copy.
func TestUnrehomeAtOpenMovesFolderHandoffsBackToTheirWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	wsDir := filepath.Join(dir, "projects")
	ws := "workspace:" + wsDir
	deep := filepath.Join(wsDir, "group", "notes")
	repo, repoKey := filepath.Join(wsDir, "tide"), filepath.Join(wsDir, "tide", ".git")
	outside := filepath.Join(dir, "elsewhere")
	workspaces := []string{wsDir}

	s, err := Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := s.DB()
	rawInsertProject(t, db, ws, wsDir, sweepT1)
	rawInsertProject(t, db, deep, deep, sweepT1)
	rawInsertProject(t, db, repoKey, repo, sweepT1)
	rawInsertProject(t, db, outside, outside, sweepT1)
	rawInsertSession(t, db, "s-deep", deep, deep, sweepT1)
	rawInsertSession(t, db, "s-repo", repoKey, repo, sweepT1)
	rawInsertSession(t, db, "s-out", outside, outside, sweepT1)
	rawInsertRecord(t, db, "h-deep", deep, "s-deep", KindHandoff, sweepT2)    // moves back
	rawInsertRecord(t, db, "d-deep", deep, "s-deep", KindDecision, sweepT2)   // not a handoff
	rawInsertRecord(t, db, "h-repo", repoKey, "s-repo", KindHandoff, sweepT2) // a repo's
	rawInsertRecord(t, db, "h-out", outside, "s-out", KindHandoff, sweepT2)   // outside every workspace
	before := dumpRecord(t, db, "h-deep")
	var seqBefore int
	if err := db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM sync_changes`).Scan(&seqBefore); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	db = s.DB()
	if got := projectKeyOf(t, db, "h-deep"); got != ws {
		t.Errorf("h-deep project_key = %q, want the workspace's %q", got, ws)
	}
	after := dumpRecord(t, db, "h-deep")
	after[0] = before[0] // project_key is the only column allowed to differ
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("h-deep column %d changed: %v -> %v", i, before[i], after[i])
		}
	}
	for id, want := range map[string]string{"d-deep": deep, "h-repo": repoKey, "h-out": outside} {
		if got := projectKeyOf(t, db, id); got != want {
			t.Errorf("%s project_key = %q, want unchanged %q", id, got, want)
		}
	}
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 1 {
		t.Errorf("pre-unrehome copies = %v, want exactly one", copies)
	}
	if _, err := db.Exec(`UPDATE records SET text = 'x' WHERE id = 'h-repo'`); err == nil {
		t.Error("records_no_update missing: UPDATE of text succeeded")
	}
	var newRows int
	var newRef string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(ref),'') FROM sync_changes WHERE seq > ?`, seqBefore).Scan(&newRows, &newRef); err != nil {
		t.Fatalf("new sync rows: %v", err)
	}
	if newRows != 1 || newRef != "h-deep" {
		t.Errorf("new sync_changes rows = %d (ref %q), want exactly one for h-deep", newRows, newRef)
	}
	if n := countWhere(t, db, "sync_changes", "ref", "h-deep"); n != 1 {
		t.Errorf("sync_changes entries for h-deep = %d, want 1", n)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second Open finds nothing to repair: no copy, no feed row.
	s, err = Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 1 {
		t.Errorf("a second Open wrote another copy: %v", copies)
	}
	var maxSeq int
	if err := s.DB().QueryRow(`SELECT MAX(seq) FROM sync_changes`).Scan(&maxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if maxSeq != seqBefore+1 {
		t.Errorf("a second Open queued more changes: max seq %d, want %d", maxSeq, seqBefore+1)
	}
	if got := projectKeyOf(t, s.DB(), "h-deep"); got != ws {
		t.Errorf("h-deep project_key = %q after a second Open, want %q", got, ws)
	}
}

// The repair files by the Workspaces configured at each Open: with the
// folder's Workspace not configured, its Handoff is already where
// HandoffHome files it (its own key), so it stays and no copy is written; the
// first Open that has the Workspace configured moves it.
func TestUnrehomeFollowsTheWorkspacesOfEachOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	wsDir := filepath.Join(dir, "projects")
	ws := "workspace:" + wsDir
	deep := filepath.Join(wsDir, "group", "notes")

	s, err := Open(path, nil, fakeSweepGit{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := s.DB()
	rawInsertProject(t, db, deep, deep, sweepT1)
	rawInsertSession(t, db, "s-deep", deep, deep, sweepT1)
	rawInsertRecord(t, db, "h-deep", deep, "s-deep", KindHandoff, sweepT2)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, nil, fakeSweepGit{})
	if err != nil {
		t.Fatalf("reopen without the workspace: %v", err)
	}
	if got := projectKeyOf(t, s.DB(), "h-deep"); got != deep {
		t.Errorf("no workspace configured: h-deep project_key = %q, want its own %q", got, deep)
	}
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 0 {
		t.Errorf("nothing to repair, yet copies = %v", copies)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, []string{wsDir}, fakeSweepGit{})
	if err != nil {
		t.Fatalf("reopen with the workspace: %v", err)
	}
	defer func() { _ = s.Close() }()
	if got := projectKeyOf(t, s.DB(), "h-deep"); got != ws {
		t.Errorf("workspace configured: h-deep project_key = %q, want %q", got, ws)
	}
}

// A store with nothing to repair, a fresh one included, gets no copy, at its
// first Open or any later one.
func TestUnrehomeWritesNoCopyWithNothingToRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	workspaces := []string{filepath.Join(dir, "projects")}
	for i := 0; i < 2; i++ {
		s, err := Open(path, workspaces, fakeSweepGit{})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		_ = s.Close()
	}
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 0 {
		t.Errorf("pre-unrehome copies = %v, want none", copies)
	}
}
