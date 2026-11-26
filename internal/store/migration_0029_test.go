package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// forgetMigration0029 makes a fully migrated store one that has not run
// migration 0029 yet.
func forgetMigration0029(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DELETE FROM schema_version WHERE version = 29`); err != nil {
		t.Fatalf("forget migration 0029: %v", err)
	}
}

// Migration 0029 (#5): the pre-#5 re-home moved a Handoff written in a non-git
// folder deeper under a Workspace onto the folder's own path, as if it were a
// repo. The migration moves it back to the Workspace's key, where
// project.HandoffHome files it, after copying the store beside itself
// (forgetting.Unrehome), and queues it for Peers. A repo's Handoff, a Handoff
// of a folder outside every Workspace and a non-Handoff stay where they are.
func TestMigration0029MovesFolderHandoffsBackToTheirWorkspace(t *testing.T) {
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
	forgetMigration0029(t, path)

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
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Run again, it finds nothing to repair: no copy, no feed row.
	forgetMigration0029(t, path)
	s, err = Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 1 {
		t.Errorf("a run with nothing to repair wrote a copy: %v", copies)
	}
	var maxSeq int
	if err := s.DB().QueryRow(`SELECT MAX(seq) FROM sync_changes`).Scan(&maxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if maxSeq != seqBefore+1 {
		t.Errorf("a run with nothing to repair queued more changes: max seq %d, want %d", maxSeq, seqBefore+1)
	}
	if got := projectKeyOf(t, s.DB(), "h-deep"); got != ws {
		t.Errorf("h-deep project_key = %q after a second run, want %q", got, ws)
	}
}

// A store with nothing to repair, a fresh one included, gets no copy.
func TestMigration0029WritesNoCopyWithNothingToRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	workspaces := []string{filepath.Join(dir, "projects")}
	s, err := Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v, err := s.Version()
	_ = s.Close()
	if err != nil || v < 29 {
		t.Errorf("schema version = %d (%v), want migration 0029 applied", v, err)
	}
	if copies, _ := filepath.Glob(path + ".pre-unrehome-*"); len(copies) != 0 {
		t.Errorf("fresh store: pre-unrehome copies = %v, want none", copies)
	}
}
