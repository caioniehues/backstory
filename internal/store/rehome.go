package store

import (
	"fmt"
	"log"
	"time"

	"github.com/RidgetopAi/backstory/internal/forgetting"
	"github.com/RidgetopAi/backstory/internal/project"
)

// rehomeCandidates selects the Handoffs the home migration considers
// (decision dc0a4387): a live (not tombstoned) kind=handoff record filed under
// a Workspace key by a Session not keyed to a Peer's placeholder. Tombstoned
// Handoffs are left alone: their text is gone and they never reached a Peer,
// so there is nothing to sync and nothing to count.
const rehomeCandidates = `SELECT r.id, s.cwd, COALESCE(s.project_key, '') FROM records r JOIN sessions s ON s.id = r.session_id
	WHERE r.kind = 'handoff' AND r.tombstoned_at IS NULL
	AND r.project_key LIKE 'workspace:%'
	AND (s.project_key IS NULL OR s.project_key NOT LIKE 'sync:%')
	ORDER BY r.id`

// rehomeMove is one Handoff the migration moves, and the key it moves to.
type rehomeMove struct{ id, newKey string }

// rehomeMoves is every Workspace-filed Handoff whose writer's Handoff home
// (project.HandoffHome, the rule note writes by) is a Project instead: a
// Handoff written in a repo under a Workspace before the rule kept it on the
// repo. A Handoff the rule itself files at a Workspace stays.
func (s *Store) rehomeMoves(workspaces []string) ([]rehomeMove, error) {
	rows, err := s.db.Query(rehomeCandidates)
	if err != nil {
		return nil, fmt.Errorf("store: rehome handoffs: candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rehomeMove
	for rows.Next() {
		var id, cwd, key string
		if err := rows.Scan(&id, &cwd, &key); err != nil {
			return nil, fmt.Errorf("store: rehome handoffs: candidates: %w", err)
		}
		if home := project.HandoffHome(cwd, key, workspaces); home != "" && !project.IsWorkspaceKey(home) {
			out = append(out, rehomeMove{id: id, newKey: home})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rehome handoffs: candidates: %w", err)
	}
	return out, nil
}

// rehomeHandoffs moves every Workspace-filed Handoff whose writer's Handoff
// home is a Project onto that Project's key (rehomeMoves), so it syncs under
// the repo's identity and counts on the repo's row. It backs the store up
// first (VACUUM INTO, the sweep pattern), then in one transaction drops
// records_no_update, rewrites project_key, recreates the trigger from its one
// definition, and queues a sync_changes row per moved record. Idempotent:
// once moved, a Handoff is no longer Workspace-filed, so a later Open writes
// neither a backup nor a row.
func (s *Store) rehomeHandoffs(workspaces []string) error {
	moves, err := s.rehomeMoves(workspaces)
	if err != nil {
		return err
	}
	n := len(moves)
	if n == 0 {
		return nil
	}

	backupPath := forgetting.PreCopyPath(s.path, forgetting.Rehome, time.Now())
	if err := s.vacuumIntoPrivate(backupPath); err != nil {
		return fmt.Errorf("store: rehome handoffs: backup to %s: %w", backupPath, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: rehome handoffs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`CREATE TEMP TABLE rehome_ids (id TEXT PRIMARY KEY, new_key TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: rehome handoffs: scratch table: %w", err)
	}
	for _, m := range moves {
		if _, err := tx.Exec(`INSERT INTO rehome_ids (id, new_key) VALUES (?, ?)`, m.id, m.newKey); err != nil {
			return fmt.Errorf("store: rehome handoffs: select %s: %w", m.id, err)
		}
	}
	// Another Open may have moved or Tombstoned one since rehomeMoves read it.
	if _, err := tx.Exec(`DELETE FROM rehome_ids WHERE id NOT IN (SELECT id FROM records
		WHERE kind = 'handoff' AND tombstoned_at IS NULL AND project_key LIKE 'workspace:%')`); err != nil {
		return fmt.Errorf("store: rehome handoffs: recheck: %w", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM rehome_ids`).Scan(&n); err != nil {
		return fmt.Errorf("store: rehome handoffs: recheck: %w", err)
	}
	if n == 0 {
		return nil
	}

	// records.project_key foreign-keys into projects(key); the session's own
	// row normally exists already, so this only guards a hand-edited store.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO projects (key, toplevel, first_seen)
		SELECT DISTINCT p.new_key, p.new_key, ? FROM rehome_ids p`, tsToNanos(time.Now())); err != nil {
		return fmt.Errorf("store: rehome handoffs: ensure projects: %w", err)
	}
	if _, err := tx.Exec(`DROP TRIGGER records_no_update`); err != nil {
		return fmt.Errorf("store: rehome handoffs: drop records_no_update: %w", err)
	}
	if _, err := tx.Exec(`UPDATE records SET project_key = (SELECT new_key FROM rehome_ids WHERE id = records.id)
		WHERE id IN (SELECT id FROM rehome_ids)`); err != nil {
		return fmt.Errorf("store: rehome handoffs: update: %w", err)
	}
	if _, err := tx.Exec(recordsNoUpdateTriggerSQL); err != nil {
		return fmt.Errorf("store: rehome handoffs: recreate records_no_update: %w", err)
	}
	// Queue each moved record for paired machines. A ref keeps one entry: the
	// older copies go after the insert (the feed only lets an entry be deleted
	// once a newer one exists).
	if _, err := tx.Exec(`INSERT INTO sync_changes (kind, ref) SELECT 'record', id FROM rehome_ids ORDER BY id`); err != nil {
		return fmt.Errorf("store: rehome handoffs: queue sync changes: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM sync_changes WHERE kind IN ('record','tombstone','session')
		AND ref IN (SELECT id FROM rehome_ids)
		AND seq < (SELECT MAX(n.seq) FROM sync_changes n WHERE n.ref = sync_changes.ref AND n.kind IN ('record','tombstone','session'))`); err != nil {
		return fmt.Errorf("store: rehome handoffs: supersede sync changes: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE temp.rehome_ids`); err != nil {
		return fmt.Errorf("store: rehome handoffs: drop scratch table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: rehome handoffs: commit: %w", err)
	}
	log.Printf("store: rehome handoffs: moved %d workspace-homed handoff(s) onto their repo key (backup %s, run by %s)", n, backupPath, processName())
	return nil
}
