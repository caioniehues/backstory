// Package forgetting is the one place that knows every stored copy of the
// store's text outside the store file itself: the backup set. Every writer of
// such a copy takes its name from here, and Purge finds the copies here, so a
// new kind of copy is reached by Purge the day it is first written. It also
// names the temporary files a copy is written through, and finds the ones a
// crash left (Leftovers) so Purge deletes those too. Record
// vectors live inside the store file and are forgotten with their Record
// (tombstoneRecordTx); they are not copies in this sense.
package forgetting

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Reason is why the store was copied beside itself before a change.
type Reason string

// The changes the store is copied before.
const (
	Restore  Reason = "restore"  // restore swaps a backup in
	Sweep    Reason = "sweep"    // the workspace project-key sweep at Open
	Identity Reason = "identity" // the repo-identity merge at Open
	Rehome   Reason = "rehome"   // the handoff re-home at Open
	// Unrehome: the repair at Open that moves back the Handoffs the pre-#5
	// re-home moved onto a non-git folder's own key under a Workspace.
	Unrehome Reason = "unrehome"
)

// preCopyLayout is the UTC stamp in a pre-copy's name.
const preCopyLayout = "20060102T150405.000000000Z"

// PreCopyPath is where the copy of the store at dbPath taken at at, before
// the change why, goes: beside the store, as <store>.pre-<why>-<UTC stamp>.
// It panics on a reason whose copy Copies would not find (why must be
// lowercase letters and hyphens), so such a writer fails its first test.
func PreCopyPath(dbPath string, why Reason, at time.Time) string {
	rest := ".pre-" + string(why) + "-" + at.UTC().Format(preCopyLayout)
	if !preCopyRest.MatchString(rest) {
		panic(fmt.Sprintf("forgetting: pre-copy reason %q would not be found by Copies", why))
	}
	return dbPath + rest
}

// backupLayout is the UTC stamp in a backup's name.
const backupLayout = "20060102T150405Z"

// BackupName is the file name of a backup `backstory backup` writes into the
// backup folder at at.
func BackupName(at time.Time) string {
	return "backstory-" + at.UTC().Format(backupLayout) + ".db"
}

// AutoBackupName is the file name of an automatic backup made at at.
func AutoBackupName(at time.Time) string {
	return autoBackupPrefix + at.UTC().Format(backupLayout) + backupSuffix
}

const (
	autoBackupPrefix = "backstory-auto-"
	backupSuffix     = ".db"
)

// AutoBackupAt is when the automatic backup named name was made; ok is false
// for any name AutoBackupName does not give.
func AutoBackupAt(name string) (at time.Time, ok bool) {
	stamp, ok := strings.CutPrefix(name, autoBackupPrefix)
	if !ok {
		return time.Time{}, false
	}
	if stamp, ok = strings.CutSuffix(stamp, backupSuffix); !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(backupLayout, stamp)
	return at, err == nil
}

// Set is where Backstory keeps copies of one store.
type Set struct {
	DBPath    string // the store file; its pre-copies sit beside it
	BackupDir string // the backup folder in effect; "" for none
}

// Copy is one stored copy of the store's text.
type Copy struct {
	Path string
	At   time.Time // its modification time
	Size int64
}

// backupName matches the names BackupName and AutoBackupName give.
var backupName = regexp.MustCompile(`^backstory-(auto-)?\d{8}T\d{6}Z\.db$`)

// preCopyRest is what follows the store's own name in a pre-copy's name, for
// any reason: older versions stamped to the second, so the fraction is
// optional.
var preCopyRest = regexp.MustCompile(`^\.pre-[a-z][a-z-]*-\d{8}T\d{6}(\.\d+)?Z$`)

// Copies lists every copy in the set, sorted by path. Only regular files
// with exactly the names Backstory gives its copies count: never a
// directory, a symlink or any other file, and nothing outside the set's
// folders.
func (s Set) Copies() ([]Copy, error) {
	var out []Copy
	if s.BackupDir != "" {
		if err := list(&out, s.BackupDir, backupName.MatchString); err != nil {
			return nil, err
		}
	}
	base := filepath.Base(s.DBPath)
	if err := list(&out, filepath.Dir(s.DBPath), func(name string) bool {
		rest, ok := strings.CutPrefix(name, base)
		return ok && preCopyRest.MatchString(rest)
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// The os.CreateTemp patterns of the temporary files that hold the store's
// text while it is being copied: a backup (store.Backup) builds its snapshot
// as BackupTemp beside where the backup goes, and restore copies the backup
// it swaps in to RestoreTemp beside the store. A copy killed mid-write
// leaves its temporary file behind.
const (
	BackupTemp  = ".backstory-backup-*.tmp"
	RestoreTemp = ".backstory-restore-*.tmp"
)

// TempMargin is how long a temporary file must go unmodified before
// Leftovers counts it as left by a crash: a backup or restore still writing
// one keeps changing it, so one inside the margin may still be in use.
const TempMargin = time.Hour

// isTempName is true for a name os.CreateTemp gives for BackupTemp or
// RestoreTemp: the pattern's prefix, a non-empty random part, its suffix.
func isTempName(name string) bool {
	for _, p := range []string{BackupTemp, RestoreTemp} {
		pre, suf, _ := strings.Cut(p, "*")
		if len(name) > len(pre)+len(suf) && strings.HasPrefix(name, pre) && strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// Leftovers lists the temporary files a backup or restore killed mid-write
// left in the set's folders (the backup folder and the store's folder),
// sorted by path: regular files with a BackupTemp or RestoreTemp name last
// modified more than TempMargin before now. A temporary file in a folder
// given to `backup --out` is not in the set.
func (s Set) Leftovers(now time.Time) ([]Copy, error) {
	var out []Copy
	dirs := []string{filepath.Dir(s.DBPath)}
	if s.BackupDir != "" && filepath.Clean(s.BackupDir) != dirs[0] {
		dirs = append(dirs, s.BackupDir)
	}
	for _, dir := range dirs {
		if err := list(&out, dir, isTempName); err != nil {
			return nil, err
		}
	}
	kept := out[:0]
	for _, c := range out {
		if now.Sub(c.At) > TempMargin {
			kept = append(kept, c)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Path < kept[j].Path })
	return kept, nil
}

// list appends the regular files in dir whose names match. A missing dir
// holds none.
func list(out *[]Copy, dir string, match func(name string) bool) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forgetting: list copies in %s: %w", dir, err)
	}
	for _, e := range ents {
		if !match(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		*out = append(*out, Copy{Path: p, At: fi.ModTime(), Size: fi.Size()})
	}
	return nil
}
