package store

import (
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RidgetopAi/backstory/internal/project"
)

// HandoffQuery names whose Handoffs Handoffs reads.
type HandoffQuery struct {
	// ProjectKey is the Project read when Dir is "" or lies outside every
	// Workspace.
	ProjectKey string
	// Dir is a folder on this Device. Under a Workspace, the Handoffs read
	// are the ones belonging to it (locationMatcher's rule: filed under its
	// repo key, or written by a Session labelled with it) and, at a
	// Workspace root itself, every Handoff homed in a repo under it too
	// (decision dc0a4387).
	Dir        string
	Git        project.Git
	Workspaces []string
	// From keeps this Device's Handoffs, a Peer's, or both.
	From OriginFilter
	// Keep, when set, drops a Handoff before its membership is decided, so
	// it must be cheap (the reader's branch).
	Keep func(Record) bool
}

// maxHandoffScan bounds Handoffs' newest-first scan, the read-side safety cap
// for a location with years of Handoff history.
const maxHandoffScan = 1000

// handoffCandidate is one Handoff id a query's SQL found. direct is true
// when it belongs to the query without a membership check.
type handoffCandidate struct {
	id     string
	ts     int64
	direct bool
}

// Handoffs yields the non-Tombstoned Handoffs q names, newest first by
// NewerHandoff's rule (ts, then id) and never by insertion order, which a
// sync scrambles. Each is read only when the caller asks for the next one.
// An error is yielded once and ends the sequence.
func (s *Store) Handoffs(q HandoffQuery) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		m, cands, err := s.handoffCandidates(q)
		if err != nil {
			yield(Record{}, err)
			return
		}
		for _, c := range cands {
			h, err := s.GetRecord(c.id)
			if err != nil {
				yield(Record{}, fmt.Errorf("store: handoffs: %w", err))
				return
			}
			if q.Keep != nil && !q.Keep(h) {
				continue
			}
			if !c.direct {
				in, err := s.handoffInLocation(m, h, q.Git, q.Workspaces)
				if err != nil {
					yield(Record{}, fmt.Errorf("store: handoffs at %s: %w", q.Dir, err))
					return
				}
				if !in {
					continue
				}
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}

// handoffCandidates is q's candidate ids, newest first, and the location
// matcher that decides the ones that are not direct.
//
// Under a Workspace it reads two keys, the Workspace's and the folder's own
// (project.Key), not project.HandoffHome, which is one of the two: that rule
// is where a Session on this Device files its Handoff, and a location also
// holds Handoffs filed under the other (#4). A repo's includes a Workspace-
// filed Handoff by a Session that edited it; a deep non-git folder's, whose
// home is the Workspace, includes a Peer's filed under the folder's own key.
// TestHandoffsAtAFolderReadItsWorkspaceAndItsOwnKey pins both.
func (s *Store) handoffCandidates(q HandoffQuery) (locationMatcher, []handoffCandidate, error) {
	dir := filepath.Clean(q.Dir)
	home, under := project.WorkspaceHome(dir, q.Workspaces)
	if q.Dir == "" || !under {
		key := s.canonicalizeProjectKey(q.ProjectKey)
		cands, err := s.handoffIDs(`project_key = ?`, []any{key}, q.From, true)
		return locationMatcher{}, cands, err
	}

	m := locationMatcher{label: project.Label(dir, q.Git, q.Workspaces)}
	keys := []any{s.canonicalizeProjectKey(home)}
	if key := project.Key(dir, q.Git, q.Workspaces); !project.IsWorkspaceKey(key) {
		m.repoKey = s.canonicalizeProjectKey(key)
		keys = append(keys, m.repoKey)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	own, err := s.handoffIDs(`project_key IN (`+marks+`)`, keys, q.From, false)
	if err != nil {
		return m, nil, err
	}
	for _, w := range q.Workspaces {
		if filepath.Clean(w) != dir {
			continue
		}
		prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)
		repos, err := s.handoffIDs(`project_key NOT LIKE 'workspace:%' AND project_key NOT LIKE 'sync:%'
			AND project_key IN (SELECT key FROM projects WHERE instr(toplevel, ?) = 1)`, []any{prefix}, q.From, true)
		if err != nil {
			return m, nil, err
		}
		own = append(own, repos...)
		sort.SliceStable(own, func(i, j int) bool {
			if own[i].ts != own[j].ts {
				return own[i].ts > own[j].ts
			}
			return own[i].id > own[j].id
		})
		if len(own) > maxHandoffScan {
			own = own[:maxHandoffScan]
		}
		break
	}
	return m, own, nil
}

// handoffIDs is the Handoffs matching where (over the records table, bound to
// args), newest first by ts then id, capped at maxHandoffScan.
func (s *Store) handoffIDs(where string, args []any, from OriginFilter, direct bool) ([]handoffCandidate, error) {
	args = append([]any{string(KindHandoff)}, args...)
	args = append(args, maxHandoffScan)
	//nolint:gosec // where and from.sql() are fixed fragments; every value is bound
	rows, err := s.db.Query(`SELECT id, ts FROM records
		WHERE kind = ? AND tombstoned_at IS NULL AND `+where+from.sql()+`
		ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: handoffs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []handoffCandidate
	for rows.Next() {
		c := handoffCandidate{direct: direct}
		if err := rows.Scan(&c.id, &c.ts); err != nil {
			return nil, fmt.Errorf("store: handoffs: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: handoffs: %w", err)
	}
	return out, nil
}

// handoffInLocation reports whether h belongs to m's location: filed under
// its repo key, or written by a Session in it (sessionInLocation).
func (s *Store) handoffInLocation(m locationMatcher, h Record, git project.Git, workspaces []string) (bool, error) {
	if m.repoKey != "" && h.ProjectKey == m.repoKey {
		return true, nil
	}
	if h.SessionID == "" {
		return false, nil
	}
	sess, err := s.getSession(h.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.sessionInLocation(m, sess, git, workspaces)
}
