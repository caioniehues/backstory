package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

const purgeMarker = "MARKER-7f3a9c-do-not-keep"

type purgeBackupFixture struct {
	f         purgeFixture
	dbPath    string
	backupDir string
	storeDir  string
	auto      []string // backups in backupDir
	manual    string
	preCopy   string // in the store dir
	preRehome string // the handoff re-home copy, in the store dir
	outFile   string // unrelated dir
	notes     string
	link      string
	outside   string // symlink target, outside backupDir
}

func fileHas(t *testing.T, path, s string) bool {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test reads a backup it just wrote
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(b, []byte(s))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// seedPurgeBackups writes a session holding purgeMarker, then every kind of
// backup around it.
func seedPurgeBackups(t *testing.T) purgeBackupFixture {
	t.Helper()
	data, dbPath := backupEnv(t)
	st, err := openStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertProject(store.Project{Key: purgeProject, Toplevel: purgeProject, FirstSeen: autoT0}); err != nil {
		t.Fatal(err)
	}
	sid, err := st.StartSession(store.StartSessionParams{Agent: "claude", HarnessSessionID: "h-marker", CWD: purgeProject, ProjectKey: purgeProject, StartedAt: autoT0, Origin: store.OriginBackfilled})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(store.Event{TS: autoT0, Kind: "tool.use", SessionID: sid, Source: "backfill", Payload: `{"text":"` + purgeMarker + `"}`}); err != nil {
		t.Fatal(err)
	}
	fx := purgeBackupFixture{dbPath: dbPath, backupDir: filepath.Join(data, "backstory", "backups"), storeDir: filepath.Dir(dbPath)}
	fx.f.a = sid
	mk := func(dest string) string {
		if _, err := st.Backup(dest, false); err != nil {
			t.Fatal(err)
		}
		if !fileHas(t, dest, purgeMarker) {
			t.Fatalf("%s should hold the marker", dest)
		}
		return dest
	}
	for _, d := range []time.Duration{0, 24 * time.Hour} {
		fx.auto = append(fx.auto, mk(filepath.Join(fx.backupDir, forgetting.AutoBackupName(autoT0.Add(d)))))
	}
	fx.manual = mk(filepath.Join(fx.backupDir, "backstory-20260302T090000Z.db"))
	fx.preCopy = mk(dbPath + ".pre-restore-20260303T090000Z")
	fx.preRehome = mk(dbPath + ".pre-rehome-20260303T090000.123456789Z")
	fx.outFile = mk(filepath.Join(t.TempDir(), "mine.db"))

	fx.notes = filepath.Join(fx.backupDir, "notes.txt")
	if err := os.WriteFile(fx.notes, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.outside = filepath.Join(t.TempDir(), "outside.db")
	if err := os.WriteFile(fx.outside, []byte("outside "+purgeMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.link = filepath.Join(fx.backupDir, "backstory-20260304T090000Z.db")
	if err := os.Symlink(fx.outside, fx.link); err != nil {
		t.Fatal(err)
	}
	return fx
}

// snapshot is name -> size+mtime for every entry in dir, for "nothing changed".
func snapshot(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			fi, err := os.Lstat(filepath.Join(d, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[filepath.Join(d, e.Name())] = fi.ModTime().String() + "/" + strconv.FormatInt(fi.Size(), 10)
		}
	}
	return out
}

func TestPurgeDeletesBackupsHoldingTextThenTakesFreshOne(t *testing.T) {
	fx := seedPurgeBackups(t)
	code, out, errs := runAutoCLI(t, "purge", "--session", fx.f.a, "--yes")
	if code != 0 {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
	if n := countRows(t, fx.dbPath, `SELECT COUNT(*) FROM timeline_events WHERE payload LIKE ?`, "%"+purgeMarker+"%"); n != 0 {
		t.Errorf("store still has the marker in %d events", n)
	}
	for _, p := range append(append([]string{}, fx.auto...), fx.manual, fx.preCopy, fx.preRehome) {
		if exists(p) {
			t.Errorf("%s survived the purge", p)
		}
	}
	if !fileHas(t, fx.outFile, purgeMarker) {
		t.Errorf("--out file was touched")
	}
	if !strings.Contains(out, purgeBackupsWrittenElsewhere) {
		t.Errorf("no written-elsewhere note: %s", out)
	}
	// exactly one new backup in backupDir, without the marker
	var fresh []string
	copies, err := forgetting.Set{DBPath: fx.dbPath, BackupDir: fx.backupDir}.Copies()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		if filepath.Dir(c.Path) == fx.backupDir {
			fresh = append(fresh, c.Path)
		}
	}
	if len(fresh) != 1 {
		t.Fatalf("new backups = %v, want exactly one", fresh)
	}
	if fileHas(t, fresh[0], purgeMarker) {
		t.Errorf("fresh backup holds the marker")
	}
	if _, err := store.VerifyBackup(fresh[0]); err != nil {
		t.Errorf("fresh backup invalid: %v", err)
	}
}

func TestPurgeDryRunListsBackupsAndDeletesNothing(t *testing.T) {
	fx := seedPurgeBackups(t)
	before := snapshot(t, fx.backupDir, fx.storeDir)
	code, out, errs := runAutoCLI(t, "purge", "--session", fx.f.a, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d err=%s", code, errs)
	}
	for _, p := range append(append([]string{}, fx.auto...), fx.manual, fx.preCopy, fx.preRehome) {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, p) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("dry-run does not list %s:\n%s", p, out)
			continue
		}
		if !strings.Contains(line, fi.ModTime().Local().Format("2006-01-02 15:04")) || !strings.Contains(line, " "+strconv.FormatInt(fi.Size(), 10)+" bytes") {
			t.Errorf("line lacks date/size: %q", line)
		}
	}
	for _, p := range []string{fx.link, fx.notes, fx.outFile} {
		if strings.Contains(out, p) {
			t.Errorf("dry-run lists %s", p)
		}
	}
	if !strings.Contains(out, purgeBackupsWrittenElsewhere) {
		t.Errorf("no written-elsewhere note:\n%s", out)
	}
	after := snapshot(t, fx.backupDir, fx.storeDir)
	for p, v := range before {
		if strings.HasPrefix(filepath.Base(p), "backstory.db") && !strings.Contains(p, ".pre-") {
			continue // the live store and its sidecars are checked by content below
		}
		if after[p] != v {
			t.Errorf("%s changed: %q -> %q", p, v, after[p])
		}
	}
	if len(after) < len(before) {
		t.Errorf("files removed: before %d after %d", len(before), len(after))
	}
	if n := countRows(t, fx.dbPath, `SELECT COUNT(*) FROM timeline_events WHERE payload LIKE ?`, "%"+purgeMarker+"%"); n != 1 {
		t.Errorf("dry-run changed the store (marker events = %d)", n)
	}
}

// The next pre-copy, for a reason no writer uses yet, is listed by the dry
// run and deleted by the purge without purge knowing the reason.
func TestPurgeReachesAPreCopyForAnyReason(t *testing.T) {
	fx := seedPurgeBackups(t)
	st, err := openStore(fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	next := forgetting.PreCopyPath(fx.dbPath, forgetting.Reason("next"), autoT0)
	if _, err := st.Backup(next, false); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if code, out, errs := runAutoCLI(t, "purge", "--session", fx.f.a, "--dry-run"); code != 0 || !strings.Contains(out, next) {
		t.Errorf("dry-run (exit %d, %s) does not list %s:\n%s", code, errs, next, out)
	}
	if code, out, errs := runAutoCLI(t, "purge", "--session", fx.f.a, "--yes"); code != 0 {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
	if exists(next) {
		t.Errorf("%s survived the purge", next)
	}
}

func TestPurgeLeavesSymlinksAndUnrelatedFiles(t *testing.T) {
	fx := seedPurgeBackups(t)
	if code, out, errs := runAutoCLI(t, "purge", "--session", fx.f.a, "--yes"); code != 0 {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
	if fi, err := os.Lstat(fx.link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink gone or replaced: %v", err)
	}
	if !fileHas(t, fx.outside, purgeMarker) {
		t.Errorf("symlink target was touched")
	}
	if !fileHas(t, fx.notes, "keep me") {
		t.Errorf("notes.txt was touched")
	}
}

func TestPurgeEraseFailureKeepsEveryBackup(t *testing.T) {
	fx := seedPurgeBackups(t)
	purgeEraseHook = func() error { return os.ErrPermission }
	t.Cleanup(func() { purgeEraseHook = nil })
	before := snapshot(t, fx.backupDir, fx.storeDir)
	code, _, _ := runAutoCLI(t, "purge", "--session", fx.f.a, "--yes")
	if code == 0 {
		t.Fatalf("purge succeeded despite failed erase")
	}
	after := snapshot(t, fx.backupDir, fx.storeDir)
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		}
	}
	if len(after) != len(before) {
		t.Errorf("a backup was written despite the failed erase")
	}
}

// dryRunBackupLine is a listed backup's date and size, which the golden
// below replaces with placeholders.
var dryRunBackupLine = regexp.MustCompile(`  \d{4}-\d\d-\d\d \d\d:\d\d  \d+ bytes$`)

// TestPurgeDryRunGolden pins every line `purge --dry-run` prints for a
// project that has backups and a paired device: the panel's Forget confirm
// reads these lines (panel/js/memory.js parseDryRun, checked against this
// golden by panel/memory_dryrun_test.go), so a wording change shows up here.
func TestPurgeDryRunGolden(t *testing.T) {
	fx := seedPurgeBackups(t)
	st, err := openStore(fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PairSyncPeer("dev-laptop", "laptop", "laptop.local"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	code, out, errs := runAutoCLI(t, "purge", "--project", purgeProject, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d err=%s", code, errs)
	}
	data := filepath.Dir(filepath.Dir(fx.dbPath))
	lines := strings.Split(strings.ReplaceAll(out, data, "$XDG_DATA_HOME"), "\n")
	for i, l := range lines {
		lines[i] = dryRunBackupLine.ReplaceAllString(l, "  <date>  <size> bytes")
	}
	compareGolden(t, "../purge/dry-run.golden", strings.Join(lines, "\n"))
}

// noRepoGit answers "not a git working tree" for every dir, so a configured
// workspace dir resolves to its workspace: key and the sweep has a target.
type noRepoGit struct{}

func (noRepoGit) Repo(string) (project.Repo, bool)   { return project.Repo{}, false }
func (noRepoGit) State(string) (project.State, bool) { return project.State{}, false }

// TestPurgeDeletesEveryCopyTheStoreKeepsBesideItself drives every writer of
// a copy beside the store while it holds purgeMarker (restore, then the
// sweep, repo-identity and handoff re-home migrations at Open) and purges
// the marker: none of those copies may survive holding the text. It uses
// the real writers rather than hand-made names, so a writer whose name purge
// does not know fails here.
func TestPurgeDeletesEveryCopyTheStoreKeepsBesideItself(t *testing.T) {
	f := newRestoreFixture(t)
	seedBackupStore(t, f.dbPath)
	st, err := store.Open(f.dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProject(store.Project{Key: purgeProject, Toplevel: purgeProject, FirstSeen: autoT0}); err != nil {
		t.Fatal(err)
	}
	sid, err := st.StartSession(store.StartSessionParams{Agent: "claude", HarnessSessionID: "h-marker", CWD: purgeProject, ProjectKey: purgeProject, StartedAt: autoT0, Origin: store.OriginBackfilled})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(store.Event{TS: autoT0, Kind: "tool.use", SessionID: sid, Source: "backfill", Payload: `{"text":"` + purgeMarker + `"}`}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// restore keeps the replaced store aside.
	if code, _, errs := f.restore(f.backup("b.db"), "--yes"); code != 0 {
		t.Fatalf("restore: exit %d: %s", code, errs)
	}

	// Rows each Open-time migration rewrites, behind a pre-copy.
	dir := t.TempDir()
	ws := filepath.Join(dir, "projects")
	wsKey := "workspace:" + ws
	repo := filepath.Join(dir, "repo")
	st, err = store.Open(f.dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ns := autoT0.UnixNano()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// sweep: a legacy plain-path row for the configured workspace dir.
		{`INSERT INTO projects (key, toplevel, first_seen) VALUES (?, ?, ?)`, []any{ws, ws, ns}},
		// identity: a remote-less repo keyed by its toplevel, common dir known.
		{`INSERT INTO projects (key, git_common_dir, toplevel, first_seen) VALUES (?, ?, ?, ?)`, []any{repo, filepath.Join(repo, ".git"), repo, ns}},
		// rehome: a handoff homed on a workspace key by a repo-keyed session.
		{`INSERT INTO projects (key, toplevel, first_seen) VALUES (?, ?, ?)`, []any{wsKey, ws, ns}},
		{`INSERT INTO sessions (id, agent, cwd, project_key, started_at, origin) VALUES ('s-repo', 'claude', ?, ?, ?, 'live')`, []any{purgeProject, purgeProject, ns}},
		{`INSERT INTO records (id, ts, kind, tier, text, about, session_id, project_key, evidence, event_cursor)
			VALUES ('h-ws', ?, 'handoff', 'agent-declared', 'handoff', '[]', 's-repo', ?, '[]', 0)`, []any{ns, wsKey}},
	} {
		if _, err := st.DB().Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	_ = st.Close()
	st, err = store.Open(f.dbPath, []string{ws}, noRepoGit{})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	copies, _ := filepath.Glob(f.dbPath + ".pre-*")
	for _, infix := range []string{".pre-restore-", ".pre-sweep-", ".pre-identity-", ".pre-rehome-"} {
		m, _ := filepath.Glob(f.dbPath + infix + "*")
		if len(m) != 1 || !fileHas(t, m[0], purgeMarker) {
			t.Fatalf("fixture: want one %s copy holding the marker, have %v (all copies: %v)", infix, m, copies)
		}
	}

	if code, out, errs := runAutoCLI(t, "purge", "--session", sid, "--yes"); code != 0 {
		t.Fatalf("purge: exit %d out=%s err=%s", code, out, errs)
	}
	left, _ := filepath.Glob(f.dbPath + ".pre-*")
	for _, p := range left {
		if fileHas(t, p, purgeMarker) {
			t.Errorf("%s survived the purge holding the text", p)
		}
	}
}
