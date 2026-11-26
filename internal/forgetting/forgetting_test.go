package forgetting

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 1, 8, 0, 0, 123456789, time.UTC)

func write(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func paths(cs []Copy) map[string]bool {
	out := map[string]bool{}
	for _, c := range cs {
		out[c.Path] = true
	}
	return out
}

// Every pre-copy named here is found here, including one for a reason no
// writer uses yet: the next pre-copy is reached without touching Purge.
func TestCopiesFindsEveryPreCopyItNames(t *testing.T) {
	db := filepath.Join(t.TempDir(), "backstory.db")
	write(t, db)
	var want []string
	for _, why := range []Reason{Restore, Sweep, Identity, Rehome, Reason("next"), Reason("re-home")} {
		want = append(want, write(t, PreCopyPath(db, why, t0)))
	}
	got, err := Set{DBPath: db}.Copies()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("found %d copies, want %d: %v", len(got), len(want), got)
	}
	for _, p := range want {
		if !paths(got)[p] {
			t.Errorf("%s not found", p)
		}
	}
}

// The backups in the backup folder are in the set: manual and automatic.
// Nothing else is: the store and its sidecars, a file merely near a copy's
// name, a directory or symlink with a copy's name, or a copy written
// somewhere else (backup --out).
func TestCopiesFindsTheBackupSetAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	db := write(t, filepath.Join(dir, "data", "backstory.db"))
	backups := filepath.Join(dir, "backups")
	want := []string{
		write(t, filepath.Join(backups, BackupName(t0))),
		write(t, filepath.Join(backups, AutoBackupName(t0))),
		write(t, PreCopyPath(db, Restore, t0)),
	}
	outside := write(t, filepath.Join(dir, "elsewhere", BackupName(t0)))
	for _, p := range []string{
		db + "-wal", db + "-shm",
		db + ".pre-restore-X",
		db + ".pre-restore-20260301T080000Z.bak",
		filepath.Join(filepath.Dir(db), "other.db.pre-restore-20260301T080000Z"),
		filepath.Join(backups, "notes.txt"),
		filepath.Join(backups, "backstory-20260301T080000Z.db.tmp"),
		filepath.Join(filepath.Dir(db), BackupName(t0)),
	} {
		write(t, p)
	}
	if err := os.Mkdir(filepath.Join(backups, BackupName(t0.Add(time.Hour))), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(backups, BackupName(t0.Add(2*time.Hour)))
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	got, err := Set{DBPath: db, BackupDir: backups}.Copies()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("found %d copies, want %d: %v", len(got), len(want), got)
	}
	for _, p := range want {
		if !paths(got)[p] {
			t.Errorf("%s not found", p)
		}
	}
	for _, c := range got {
		if c.Size != int64(len("text")) || c.At.IsZero() {
			t.Errorf("%s: size %d at %v", c.Path, c.Size, c.At)
		}
	}
}

// A backup folder that is not there yet holds no copies.
func TestCopiesOfAMissingBackupFolderIsNone(t *testing.T) {
	dir := t.TempDir()
	got, err := Set{DBPath: filepath.Join(dir, "backstory.db"), BackupDir: filepath.Join(dir, "none")}.Copies()
	if err != nil || len(got) != 0 {
		t.Fatalf("Copies = %v, %v; want none", got, err)
	}
}

// An automatic backup's name gives back when it was made, to the second; no
// other name is an automatic backup's.
func TestAutoBackupAtReadsOnlyAutomaticBackupNames(t *testing.T) {
	at, ok := AutoBackupAt(AutoBackupName(t0))
	if want := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC); !ok || !at.Equal(want) {
		t.Errorf("AutoBackupAt(%s) = %v, %v; want %v", AutoBackupName(t0), at, ok, want)
	}
	for _, name := range []string{
		BackupName(t0),
		"backstory-auto-20260301T080000Z.db.tmp",
		"backstory-auto-2026-03-01.db",
		"backstory-auto-.db",
	} {
		if at, ok := AutoBackupAt(name); ok {
			t.Errorf("AutoBackupAt(%s) = %v, true; want false", name, at)
		}
	}
}

// A reason whose copy Copies could not find is refused when the copy is
// named, so the writer's first test fails instead of a purge missing it.
func TestPreCopyPathRefusesAReasonCopiesCouldNotFind(t *testing.T) {
	for _, why := range []Reason{"", "V2", "re_home", "-sweep", "a/b"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("PreCopyPath(%q) did not panic", why)
				}
			}()
			_ = PreCopyPath("/data/backstory.db", why, t0)
		}()
	}
}
