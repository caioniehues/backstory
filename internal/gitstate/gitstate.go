// Package gitstate is the one rule for the latest git state per working tree:
// which session.git_state events count as a Session's end state, which
// working tree each one is of, and which is a tree's latest. The SessionStart
// block's Repo line and This Week's uncommitted items both read it (#4).
package gitstate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

// SessionEnd is a working tree's git state as observed when a Session ended
// there.
type SessionEnd struct {
	// Tree is the working tree's toplevel on this Device (store.SessionTree);
	// "" for a Peer's Session or one with no absolute folder, which name no
	// tree here and share one slot.
	Tree string
	// Event is the session.git_state event the state was read from.
	Event store.TimelineEvent
	// Branch is the branch checked out; "" when git named none.
	Branch string
	// Uncommitted is the count of uncommitted and untracked files.
	Uncommitted int
}

// At is when the state was observed.
func (s SessionEnd) At() time.Time { return s.Event.TS }

// observed is e's end state when it is one: a session.git_state event that
// is not the session-start observation, from a Session that is not a shell's
// (older daemons left per-command shell sessions on disk, each with a git
// state of its own), and that observed something. A could-not-observe event
// carries no evidence either way (SCHEMA.md invariant 7) and never counts,
// so a tree's latest state is its newest OBSERVED one; a payload that does
// not parse observed nothing either.
func observed(e store.TimelineEvent, shell map[string]bool) (payload.SessionGitState, bool) {
	if e.Kind != payload.KindSessionGitState || shell[e.SessionID] {
		return payload.SessionGitState{}, false
	}
	var gs payload.SessionGitState
	if json.Unmarshal([]byte(e.Payload), &gs) != nil {
		return payload.SessionGitState{}, false
	}
	if gs.Phase == payload.GitStatePhaseStart || gs.CouldNotObserve || gs.UncommittedCount == nil {
		return payload.SessionGitState{}, false
	}
	return gs, true
}

// trees resolves each Session's working tree once, through one per-call git
// cache (project.PerCall), so a read costs one git question per distinct
// folder, never one per Session, whichever Git its caller holds.
type trees struct {
	st  *store.Store
	git project.Git
	of  map[string]string
}

func newTrees(st *store.Store, git project.Git) *trees {
	return &trees{st: st, git: project.PerCall(git), of: map[string]string{}}
}

func (t *trees) tree(sessionID string) (string, error) {
	if tree, ok := t.of[sessionID]; ok {
		return tree, nil
	}
	tree, err := t.st.SessionTree(t.git, sessionID)
	if err != nil {
		return "", fmt.Errorf("gitstate: tree of session %s: %w", sessionID, err)
	}
	t.of[sessionID] = tree
	return tree, nil
}

// Latest is each working tree's latest observed session-end state among
// events (oldest first, as the store returns them), one per tree, in the
// order the trees first appear. A tree whose events are all
// could-not-observe has none.
func Latest(st *store.Store, git project.Git, events []store.TimelineEvent) ([]SessionEnd, error) {
	shell, err := st.ShellSessionIDs()
	if err != nil {
		return nil, fmt.Errorf("gitstate: %w", err)
	}
	ts := newTrees(st, git)
	latest := map[string]SessionEnd{}
	var order []string
	for _, e := range events {
		gs, ok := observed(e, shell)
		if !ok {
			continue
		}
		tree, err := ts.tree(e.SessionID)
		if err != nil {
			return nil, err
		}
		if _, seen := latest[tree]; !seen {
			order = append(order, tree)
		}
		latest[tree] = sessionEnd(tree, e, gs)
	}
	out := make([]SessionEnd, 0, len(order))
	for _, tree := range order {
		out = append(out, latest[tree])
	}
	return out, nil
}

// LatestIn is tree's latest observed session-end state among events (oldest
// first); with tree "", the newest observed one in any tree, for a reader
// with no folder of its own, whose Tree is then left unresolved (""). It
// reads newest first and stops at the first match, so the trees of older
// Sessions are never resolved.
func LatestIn(st *store.Store, git project.Git, events []store.TimelineEvent, tree string) (SessionEnd, bool, error) {
	shell, err := st.ShellSessionIDs()
	if err != nil {
		return SessionEnd{}, false, fmt.Errorf("gitstate: %w", err)
	}
	ts := newTrees(st, git)
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		gs, ok := observed(e, shell)
		if !ok {
			continue
		}
		at := ""
		if tree != "" {
			if at, err = ts.tree(e.SessionID); err != nil {
				return SessionEnd{}, false, err
			}
			if at != tree {
				continue
			}
		}
		return sessionEnd(at, e, gs), true, nil
	}
	return SessionEnd{}, false, nil
}

func sessionEnd(tree string, e store.TimelineEvent, gs payload.SessionGitState) SessionEnd {
	return SessionEnd{Tree: tree, Event: e, Branch: gs.Branch, Uncommitted: *gs.UncommittedCount}
}
