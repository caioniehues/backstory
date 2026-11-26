package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestRehomeHandoffsMigration is DONE WHEN clause 3 (decision dc0a4387).
//
// Mutation probes: drop the sync_changes insert in rehomeHandoffs -> the
// "one sync_changes row per moved record" assertion fails; skip the trigger
// recreate -> the append-only assertion fails.
func TestRehomeHandoffsMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	ws := "workspace:" + filepath.Join(dir, "projects")
	repo := filepath.Join(dir, "projects", "tide")

	s, err := Open(path, nil, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := s.DB()
	rawInsertProject(t, db, ws, ws, sweepT1)
	rawInsertProject(t, db, repo, repo, sweepT1)
	rawInsertSession(t, db, "s-repo", repo, repo, sweepT1)
	rawInsertSession(t, db, "s-ws", ws, filepath.Join(dir, "projects"), sweepT1)
	rawInsertRecord(t, db, "h-a", ws, "s-repo", KindHandoff, sweepT2) // moves
	rawInsertRecord(t, db, "h-b", ws, "s-ws", KindHandoff, sweepT2)   // stays
	rawInsertRecord(t, db, "h-c", repo, "s-repo", KindHandoff, sweepT2)
	rawInsertRecord(t, db, "d-a", ws, "s-repo", KindDecision, sweepT2) // not a handoff
	before := dumpRecord(t, db, "h-a")
	var seqBefore int
	if err := db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM sync_changes`).Scan(&seqBefore); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, nil, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	db = s.DB()

	if got := projectKeyOf(t, db, "h-a"); got != repo {
		t.Errorf("(a) project_key = %q, want %q", got, repo)
	}
	after := dumpRecord(t, db, "h-a")
	after[0] = before[0] // project_key is the only column allowed to differ
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("(a) column %d changed: %v -> %v", i, before[i], after[i])
		}
	}
	if got := projectKeyOf(t, db, "h-b"); got != ws {
		t.Errorf("(b) project_key = %q, want unchanged %q", got, ws)
	}
	if got := projectKeyOf(t, db, "h-c"); got != repo {
		t.Errorf("(c) project_key = %q, want unchanged %q", got, repo)
	}
	if got := projectKeyOf(t, db, "d-a"); got != ws {
		t.Errorf("non-handoff moved to %q", got)
	}

	backups, _ := filepath.Glob(path + ".pre-rehome-*")
	if len(backups) != 1 {
		t.Fatalf("pre-rehome backups = %v, want exactly one", backups)
	}
	if _, err := db.Exec(`UPDATE records SET text = 'x' WHERE id = 'h-c'`); err == nil {
		t.Error("records_no_update missing: UPDATE of text succeeded")
	}
	if n := countWhere(t, db, "sync_changes", "ref", "h-a"); n != 1 {
		t.Errorf("sync_changes rows for the moved record = %d, want 1", n)
	}
	// Exactly one new feed row appeared, for the moved record, and the
	// record keeps a single entry.
	var newRows int
	var newRef string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(ref),'') FROM sync_changes WHERE seq > ?`, seqBefore).Scan(&newRows, &newRef); err != nil {
		t.Fatalf("new sync rows: %v", err)
	}
	if newRows != 1 || newRef != "h-a" {
		t.Errorf("new sync_changes rows = %d (ref %q), want exactly one for h-a", newRows, newRef)
	}
	if n := countWhere(t, db, "sync_changes", "ref", "h-a"); n != 1 {
		t.Errorf("sync_changes entries for h-a = %d, want 1", n)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = Open(path, nil, nil)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	backups, _ = filepath.Glob(path + ".pre-rehome-*")
	if len(backups) != 1 {
		t.Errorf("second Open wrote another backup: %v", backups)
	}
	var maxSeq int
	if err := s.DB().QueryRow(`SELECT MAX(seq) FROM sync_changes`).Scan(&maxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if maxSeq != seqBefore+1 {
		t.Errorf("second Open queued more changes: max seq %d", maxSeq)
	}
}

func projectKeyOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var k string
	if err := db.QueryRow(`SELECT project_key FROM records WHERE id = ?`, id).Scan(&k); err != nil {
		t.Fatalf("project_key of %s: %v", id, err)
	}
	return k
}

// dumpRecord is every column of a record as strings, in table order.
func dumpRecord(t *testing.T, db *sql.DB, id string) []any {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM records WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("dump %s: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	if !rows.Next() {
		t.Fatalf("record %s missing", id)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan %s: %v", id, err)
	}
	// project_key goes first so a caller can blank exactly that column.
	for i, c := range cols {
		if c == "project_key" {
			vals[0], vals[i] = vals[i], vals[0]
			break
		}
	}
	return vals
}

// TestRehomeLeavesEveryHandoffWhereTheWriteRuleFiledIt: the migration at Open
// only moves a Handoff its writer would no longer file where it is (#5). A
// Handoff note filed under its writer's Handoff home (project.HandoffHome)
// stays put — in particular one written in a non-git folder deeper under a
// Workspace, whose Session is keyed by its bare folder but whose Handoffs home
// at the Workspace (decision dc0a4387). Only the legacy one, filed under the
// Workspace by a Session in a repo under it, moves onto the repo.
func TestRehomeLeavesEveryHandoffWhereTheWriteRuleFiledIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backstory.db")
	root := filepath.Join(dir, "projects")
	workspaces := []string{root}
	ws := "workspace:" + root
	repo := filepath.Join(root, "tide")
	repoKey := filepath.Join(repo, ".git")
	direct := filepath.Join(root, "scratch")
	deep := filepath.Join(root, "group", "plain")

	s, err := Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := s.DB()
	rawInsertProject(t, db, ws, root, sweepT1)
	rawInsertProject(t, db, repoKey, repo, sweepT1)
	rawInsertProject(t, db, deep, deep, sweepT1)
	rawInsertSession(t, db, "s-root", ws, root, sweepT1)
	rawInsertSession(t, db, "s-direct", ws, direct, sweepT1)
	rawInsertSession(t, db, "s-deep", deep, deep, sweepT1)
	rawInsertSession(t, db, "s-repo", repoKey, repo, sweepT1)
	rawInsertRecord(t, db, "h-root", ws, "s-root", KindHandoff, sweepT2)
	rawInsertRecord(t, db, "h-direct", ws, "s-direct", KindHandoff, sweepT2)
	rawInsertRecord(t, db, "h-deep", ws, "s-deep", KindHandoff, sweepT2)
	rawInsertRecord(t, db, "h-repo", repoKey, "s-repo", KindHandoff, sweepT2)
	rawInsertRecord(t, db, "h-legacy", ws, "s-repo", KindHandoff, sweepT2)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, workspaces, fakeSweepGit{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	for id, want := range map[string]string{
		"h-root": ws, "h-direct": ws, "h-deep": ws, "h-repo": repoKey, "h-legacy": repoKey,
	} {
		if got := projectKeyOf(t, s.DB(), id); got != want {
			t.Errorf("%s project_key = %q, want %q", id, got, want)
		}
	}
}
