package store

import (
	"fmt"
	"log"
	"time"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/project"
)

// unrehomeCandidates selects the Handoffs the repair at Open considers: live
// Handoffs written on this Device, filed under their writing Session's own
// key, where that key is neither a Workspace nor a Peer's placeholder. That is
// where the pre-#5 re-home moved a Workspace-filed Handoff. A Peer's Handoff
// is never one: a Peer's Session joins a home only by its own key
// (project.InHome), so its folder path, a path on the Peer, files nothing here.
const unrehomeCandidates = `SELECT r.id, s.cwd, s.project_key FROM records r JOIN sessions s ON s.id = r.session_id
	WHERE r.kind = 'handoff' AND r.tombstoned_at IS NULL AND r.origin_device IS NULL
	AND r.project_key = s.project_key
	AND r.project_key NOT LIKE 'workspace:%' AND r.project_key NOT LIKE 'sync:%'
	ORDER BY r.id`

// unrehomeMoves is every candidate whose writer's Handoff home
// (project.HandoffHome, with workspaces) is a Workspace: its key is a bare
// folder path under that Workspace, not a repo. Each moves to that
// Workspace's key.
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

// unrehomeHandoffs is the repair the re-home's pre-#5 rule needs, run at
// every Open like rehomeHandoffs: it moves every Handoff unrehomeMoves finds,
// with the Workspaces configured at this Open, back onto its Workspace's key.
// Only when there is one to move is the store first copied beside itself
// (forgetting.Unrehome); then, in one transaction that re-reads the
// candidates, records_no_update is dropped for the UPDATE and recreated from
// its one definition, and each moved record is queued for Peers as the
// re-home queues it. Idempotent: once moved, a Handoff is no longer filed
// under its writer's own key, so a later Open writes neither a copy nor a
// row.
func (s *Store) unrehomeHandoffs(workspaces []string) error {
	moves, err := unrehomeMoves(s.db, workspaces)
	if err != nil || len(moves) == 0 {
		return err
	}
	backupPath := forgetting.PreCopyPath(s.path, forgetting.Unrehome, time.Now())
	if err := s.vacuumIntoPrivate(backupPath); err != nil {
		return fmt.Errorf("store: unrehome handoffs: backup to %s: %w", backupPath, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: unrehome handoffs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Another Open may have moved or Tombstoned one since the read above.
	if moves, err = unrehomeMoves(tx, workspaces); err != nil || len(moves) == 0 {
		return err
	}
	now := tsToNanos(time.Now())
	for _, m := range moves {
		// records.project_key foreign-keys into projects(key): the Workspace's
		// own row normally exists already.
		top, _ := project.WorkspaceDirOf(m.newKey)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO projects (key, toplevel, first_seen) VALUES (?, ?, ?)`,
			m.newKey, top, now); err != nil {
			return fmt.Errorf("store: unrehome handoffs: ensure project %s: %w", m.newKey, err)
		}
	}
	if _, err := tx.Exec(`DROP TRIGGER records_no_update`); err != nil {
		return fmt.Errorf("store: unrehome handoffs: drop records_no_update: %w", err)
	}
	for _, m := range moves {
		if _, err := tx.Exec(`UPDATE records SET project_key = ? WHERE id = ?`, m.newKey, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: move %s: %w", m.id, err)
		}
		// A ref keeps one feed entry: the older ones go after the insert.
		if _, err := tx.Exec(`INSERT INTO sync_changes (kind, ref) VALUES ('record', ?)`, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: queue %s: %w", m.id, err)
		}
		if _, err := tx.Exec(`DELETE FROM sync_changes WHERE kind IN ('record','tombstone','session') AND ref = ?
			AND seq < (SELECT MAX(n.seq) FROM sync_changes n WHERE n.ref = ? AND n.kind IN ('record','tombstone','session'))`, m.id, m.id); err != nil {
			return fmt.Errorf("store: unrehome handoffs: supersede %s: %w", m.id, err)
		}
	}
	if _, err := tx.Exec(recordsNoUpdateTriggerSQL); err != nil {
		return fmt.Errorf("store: unrehome handoffs: recreate records_no_update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: unrehome handoffs: commit: %w", err)
	}
	log.Printf("store: unrehome handoffs: moved %d handoff(s) back to their workspace (backup %s, run by %s)", len(moves), backupPath, processName())
	return nil
}
