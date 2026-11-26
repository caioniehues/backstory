package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/store"
)

// purgeEraseHook, when set (tests only), runs just before the store erase and
// a non-nil return fails the purge as if the erase itself had failed.
var purgeEraseHook func() error

// purgeBackupsWrittenElsewhere is the line purge prints about `backup --out`.
const purgeBackupsWrittenElsewhere = "backups written elsewhere with --out are not touched and still hold the text, so delete them yourself."

// purgeFiles are the files Backstory wrote that a purge deletes, because they
// still hold the text.
type purgeFiles struct {
	backups   []forgetting.Copy // the backup set
	leftovers []forgetting.Copy // temp files a crashed backup or restore left
}

// purgeBackups lists the files Backstory wrote that purge deletes, at now:
// the backup set of the store at dbPath with backupDir in effect, and the
// temp files a crashed backup or restore left there (forgetting knows every
// name, and how old a temp file must be before it counts as left).
func purgeBackups(dbPath, backupDir string, now time.Time) (purgeFiles, error) {
	set := forgetting.Set{DBPath: dbPath, BackupDir: backupDir}
	backups, err := set.Copies()
	if err != nil {
		return purgeFiles{}, err
	}
	leftovers, err := set.Leftovers(now)
	if err != nil {
		return purgeFiles{}, err
	}
	return purgeFiles{backups: backups, leftovers: leftovers}, nil
}

// printPurgeBackups lists what a purge would delete, then the --out note.
// The leftovers line appears only when there are any.
func printPurgeBackups(w io.Writer, files purgeFiles) {
	if len(files.backups) == 0 {
		_, _ = fmt.Fprintln(w, "no backups made by Backstory hold this text")
	} else {
		_, _ = fmt.Fprintf(w, "would also delete %d backups Backstory made (they still hold the text):\n", len(files.backups))
		printPurgeFiles(w, files.backups)
	}
	if len(files.leftovers) > 0 {
		_, _ = fmt.Fprintf(w, "would also delete %d temporary files a stopped backup or restore left (they may hold the text):\n", len(files.leftovers))
		printPurgeFiles(w, files.leftovers)
	}
	_, _ = fmt.Fprintln(w, purgeBackupsWrittenElsewhere)
}

// printPurgeFiles prints one indented line per file: path, date and size.
func printPurgeFiles(w io.Writer, files []forgetting.Copy) {
	for _, f := range files {
		_, _ = fmt.Fprintf(w, "  %s  %s  %d bytes\n", f.Path, f.At.Local().Format("2006-01-02 15:04"), f.Size)
	}
}

// cleanBackupsAfterPurge runs after the store erase succeeded: it deletes the
// backups Backstory wrote and the temp files a crashed backup or restore left
// (re-listed now, so none is missed) and takes a fresh backup so the user is
// never left with none. It returns false only when a file could not be
// deleted; a failed fresh backup is a loud warning, not a failure, because
// the purge itself succeeded.
func cleanBackupsAfterPurge(st *store.Store, dbPath string, stdout, stderr io.Writer) bool {
	ok := true
	dir, err := effectiveBackupDir(st)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but the backup folder could not be read, so old backups still hold the purged text and no fresh backup was taken:", err)
		return false
	}
	files, err := purgeBackups(dbPath, dir, time.Now())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but old backups could not be listed, so they still hold the purged text:", err)
		ok = false
	}
	remove := func(fs []forgetting.Copy) int {
		deleted := 0
		for _, f := range fs {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintf(stderr, "backstory purge: WARNING: could not delete %s, it still holds the purged text: %v\n", f.Path, err)
				ok = false
				continue
			}
			deleted++
		}
		return deleted
	}
	_, _ = fmt.Fprintf(stdout, "deleted %d backups that held the purged text\n", remove(files.backups))
	if len(files.leftovers) > 0 {
		_, _ = fmt.Fprintf(stdout, "deleted %d temporary files a stopped backup or restore left\n", remove(files.leftovers))
	}
	_, _ = fmt.Fprintln(stdout, purgeBackupsWrittenElsewhere)

	info, err := writeDefaultBackup(st)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but the fresh backup failed, so you have no backup right now:", err)
		return ok
	}
	_, _ = fmt.Fprintf(stdout, "fresh backup: %s\n", info.Path)
	return ok
}
