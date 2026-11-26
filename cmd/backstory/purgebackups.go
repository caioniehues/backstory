package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/store"
)

// purgeEraseHook, when set (tests only), runs just before the store erase and
// a non-nil return fails the purge as if the erase itself had failed.
var purgeEraseHook func() error

// purgeBackupsWrittenElsewhere is the line purge prints about `backup --out`.
const purgeBackupsWrittenElsewhere = "backups written elsewhere with --out are not touched and still hold the text, so delete them yourself."

// purgeBackups lists the copies Backstory wrote that purge deletes: the
// backup set of the store at dbPath with backupDir in effect (forgetting
// knows every name).
func purgeBackups(dbPath, backupDir string) ([]forgetting.Copy, error) {
	return forgetting.Set{DBPath: dbPath, BackupDir: backupDir}.Copies()
}

// printPurgeBackups lists what a purge would delete, then the --out note.
func printPurgeBackups(w io.Writer, files []forgetting.Copy) {
	if len(files) == 0 {
		_, _ = fmt.Fprintln(w, "no backups made by Backstory hold this text")
	} else {
		_, _ = fmt.Fprintf(w, "would also delete %d backups Backstory made (they still hold the text):\n", len(files))
		for _, f := range files {
			_, _ = fmt.Fprintf(w, "  %s  %s  %d bytes\n", f.Path, f.At.Local().Format("2006-01-02 15:04"), f.Size)
		}
	}
	_, _ = fmt.Fprintln(w, purgeBackupsWrittenElsewhere)
}

// cleanBackupsAfterPurge runs after the store erase succeeded: it deletes the
// backups Backstory wrote (re-listed now, so none is missed) and takes a
// fresh one so the user is never left with none. It returns false only when a
// backup could not be deleted; a failed fresh backup is a loud warning, not a
// failure, because the purge itself succeeded.
func cleanBackupsAfterPurge(st *store.Store, dbPath string, stdout, stderr io.Writer) bool {
	ok := true
	dir, err := effectiveBackupDir(st)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but the backup folder could not be read, so old backups still hold the purged text and no fresh backup was taken:", err)
		return false
	}
	files, err := purgeBackups(dbPath, dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but old backups could not be listed, so they still hold the purged text:", err)
		ok = false
	}
	deleted := 0
	for _, f := range files {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "backstory purge: WARNING: could not delete %s, it still holds the purged text: %v\n", f.Path, err)
			ok = false
			continue
		}
		deleted++
	}
	_, _ = fmt.Fprintf(stdout, "deleted %d backups that held the purged text\n", deleted)
	_, _ = fmt.Fprintln(stdout, purgeBackupsWrittenElsewhere)

	info, err := writeDefaultBackup(st)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory purge: WARNING: the purge worked, but the fresh backup failed, so you have no backup right now:", err)
		return ok
	}
	_, _ = fmt.Fprintf(stdout, "fresh backup: %s\n", info.Path)
	return ok
}
