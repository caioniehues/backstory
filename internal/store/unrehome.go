package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/project"
)

// migrationStoreHooks are data hooks that need the Store's configuration
// (its Workspaces), run like migrationDataHooks inside the migration's own
// transaction.
var migrationStoreHooks = map[int]func(*Store, context.Context, *sql.Tx) error{
	29: (*Store).unrehomeFolderHandoffs,
}

// migrationPreCopies run before a migration's transaction, when it is about
// to be applied: a copy of the store beside itself, written only when the
// migration has something to change.
var migrationPreCopies = map[int]func(*Store) error{
	29: (*Store).preCopyForUnrehome,
}

// unrehomeCandidates selects the Handoffs migration 0029 considers: live
// Handoffs filed under their writing Session's own key, where that key is
// neither a Workspace nor a Peer's placeholder. That is where the pre-#5
// re-home moved a Workspace-filed Handoff.
const unrehomeCandidates = `SELECT r.id, s.cwd, s.project_key FROM records r JOIN sessions s ON s.id = r.session_id
	WHERE r.kind = 'handoff' AND r.tombstoned_at IS NULL
	AND r.project_key = s.project_key
	AND r.project_key NOT LIKE 'workspace:%' AND r.project_key NOT LIKE 'sync:%'
	ORDER BY r.id`

// unrehomeMoves is every candidate whose writer's Handoff home
// (project.HandoffHome) is a configured Workspace: its key is a bare folder
// path under that Workspace, not a repo. Each moves to that Workspace's key.
func unrehomeMoves(q purgeQuerier, workspaces []string) ([]rehomeMove, error) {
	if len(workspaces) == 0 {
		return nil, nil
	}
	rows, err := q.Query(unrehomeCandidates)
	if err != nil {
		return nil, fmt.Errorf("store: unrehome handoffs: candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rehomeMove
	for rows.Next() {
		var id, cwd, key string
		if err := rows.Scan(&id, &cwd, &key); err != nil {
			return nil, fmt.Errorf("store: unrehome handoffs: candidates: %w", err)
		}
		if home := project.HandoffHome(cwd, key, workspaces); project.IsWorkspaceKey(home) && home != key {
			out = append(out, rehomeMove{id: id, newKey: home})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: unrehome handoffs: candidates: %w", err)
	}
	return out, nil
}

// preCopyForUnrehome copies the store beside itself (forgetting.Unrehome)
// when migration 0029 has a Handoff to move, and writes nothing otherwise.
func (s *Store) preCopyForUnrehome() error {
	moves, err := unrehomeMoves(s.db, s.workspaces)
	if err != nil || len(moves) == 0 {
		return err
	}
	p := forgetting.PreCopyPath(s.path, forgetting.Unrehome, time.Now())
	if err := s.vacuumIntoPrivate(p); err != nil {
		return fmt.Errorf("store: unrehome handoffs: backup to %s: %w", p, err)
	}
	log.Printf("store: unrehome handoffs: %d handoff(s) to move back to their workspace (backup %s, run by %s)", len(moves), p, processName())
	return nil
}

// unrehomeFolderHandoffs is migration 0029's store hook: it moves every
// Handoff unrehomeMoves finds back onto its Workspace's key, re-reading the
// candidates inside the migration's transaction. records_no_update is dropped
// for the one UPDATE and recreated from recordsNoUpdateTriggerSQL (the
// trigger as of this migration; a later migration that changes the trigger
// must freeze this version's copy here), and each moved record is queued for
// Peers as the re-home queues it. With nothing to move it changes nothing.
func (s *Store) unrehomeFolderHandoffs(ctx context.Context, tx *sql.Tx) error {
	moves, err := unrehomeMoves(tx, s.workspaces)
	if err != nil || len(moves) == 0 {
		return err
	}
	now := tsToNanos(time.Now())
	for _, m := range moves {
		// records.project_key foreign-keys into projects(key): the Workspace's
		// own row normally exists already.
		top, _ := project.WorkspaceDirOf(m.newKey)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projects (key, toplevel, first_seen) VALUES (?, ?, ?)`,
			m.newKey, top, now); err != nil {
			return fmt.Errorf("store: unrehome handoffs: ensure project %s: %w", m.newKey, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER records_no_update`); err != nil {
		return fmt.Errorf("store: unrehome handoffs: drop records_no_update: %w", err)
	}
	for _, m := range moves {
		if _, err := tx.ExecContext(ctx, `UPDATE records SET project_key = ? WHERE id = ?`, m.newKey, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: move %s: %w", m.id, err)
		}
		// A ref keeps one feed entry: the older ones go after the insert.
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_changes (kind, ref) VALUES ('record', ?)`, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: queue %s: %w", m.id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_changes WHERE kind IN ('record','tombstone','session') AND ref = ?
			AND seq < (SELECT MAX(n.seq) FROM sync_changes n WHERE n.ref = ? AND n.kind IN ('record','tombstone','session'))`, m.id, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: supersede %s: %w", m.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, recordsNoUpdateTriggerSQL); err != nil {
		return fmt.Errorf("store: unrehome handoffs: recreate records_no_update: %w", err)
	}
	log.Printf("store: unrehome handoffs: moved %d handoff(s) back to their workspace", len(moves))
	return nil
}
