package handoff

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

// Reader is who asks for the Resume handoff: a folder on this Device, the
// Project it is in, and the branch checked out there.
type Reader struct {
	ProjectKey string
	// Dir is the reader's folder on this Device, never a Peer's: a Peer's
	// path means nothing here. "" reads ProjectKey's own Handoffs.
	Dir string
	// Branch is the branch checked out at Dir; "" when unknown, which
	// prefers no branch.
	Branch     string
	Git        project.Git
	Workspaces []string
}

// Resume is the Resume handoff a Reader is offered.
type Resume struct {
	Handoff store.Record
	Found   bool
	// OtherBranch: the reader's branch is known, no Handoff is on it, and
	// Handoff was written on another known branch.
	OtherBranch bool
	// Also is the other Device's newest Handoff (this Device's when Handoff
	// is a Peer's, a Peer's otherwise) when it is Current; nil when it is
	// superseded or there is none.
	Also *store.Record
}

// ResumeFor is the Resume handoff for r: the newest Current Handoff (never
// superseded, never Tombstoned) on the reader's branch, else the newest
// Current one in the Project, by store.NewerHandoff, this Device's and its
// Peers' alike. The SessionStart block resumes it and This Week lists it.
//
// Which Handoffs are the reader's is store.Handoffs' location rule at Dir.
// At a non-git folder directly under a Workspace, which shares the root's
// key, with no Handoff of its own the root's are read (decision 7a2556b6).
func ResumeFor(st *store.Store, r Reader) (Resume, error) {
	if r.Branch != "" {
		res, err := resumeWhere(st, r, func(h store.Record) bool { return h.GitBranch == "" || h.GitBranch == r.Branch })
		if err != nil || res.Found {
			return res, err
		}
	}
	res, err := resumeWhere(st, r, nil)
	if err == nil && res.Found && r.Branch != "" && res.Handoff.GitBranch != "" && res.Handoff.GitBranch != r.Branch {
		res.OtherBranch = true
	}
	return res, err
}

// Newest is the newest Current Handoff among recs by store.NewerHandoff,
// whatever their order: recall's stand-in when nothing matches.
func Newest(st *store.Store, recs []store.Record) (store.Record, bool, error) {
	hs := make([]store.Record, 0, len(recs))
	for _, r := range recs {
		if r.Kind == store.KindHandoff && r.TombstonedAt == nil {
			hs = append(hs, r)
		}
	}
	sort.SliceStable(hs, func(i, j int) bool { return store.NewerHandoff(hs[i], hs[j]) })
	for _, h := range hs {
		sup, err := st.IsSuperseded(h.ID)
		if err != nil {
			return store.Record{}, false, fmt.Errorf("handoff: newest: %w", err)
		}
		if !sup {
			return h, true, nil
		}
	}
	return store.Record{}, false, nil
}

// newestOf is one Device's newest Handoff at a place, and its newest
// Current one.
type newestOf struct {
	newest, current       store.Record
	hasNewest, hasCurrent bool
}

func resumeWhere(st *store.Store, r Reader, keep func(store.Record) bool) (Resume, error) {
	local, err := newestFrom(st, r, store.OriginLocal, keep)
	if err != nil {
		return Resume{}, err
	}
	peer, err := newestFrom(st, r, store.OriginImported, keep)
	if err != nil {
		return Resume{}, err
	}
	pick, other := local, peer
	if !local.hasCurrent || (peer.hasCurrent && store.NewerHandoff(peer.current, local.current)) {
		pick, other = peer, local
	}
	if !pick.hasCurrent {
		return Resume{}, nil
	}
	res := Resume{Handoff: pick.current, Found: true}
	if other.hasCurrent && other.current.ID == other.newest.ID {
		also := other.newest
		res.Also = &also
	}
	return res, nil
}

// newestFrom reads one origin's Handoffs at the reader's folder, else (a
// non-git child of a Workspace with none) at the root.
func newestFrom(st *store.Store, r Reader, from store.OriginFilter, keep func(store.Record) bool) (newestOf, error) {
	for _, dir := range places(r) {
		var n newestOf
		q := store.HandoffQuery{ProjectKey: r.ProjectKey, Dir: dir, Git: r.Git, Workspaces: r.Workspaces, From: from, Keep: keep}
		for h, err := range st.Handoffs(q) {
			if err != nil {
				return newestOf{}, fmt.Errorf("handoff: resume: %w", err)
			}
			if !n.hasNewest {
				n.newest, n.hasNewest = h, true
			}
			sup, err := st.IsSuperseded(h.ID)
			if err != nil {
				return newestOf{}, fmt.Errorf("handoff: resume: %w", err)
			}
			if !sup {
				n.current, n.hasCurrent = h, true
				break
			}
		}
		if n.hasNewest {
			return n, nil
		}
	}
	return newestOf{}, nil
}

// places is where the reader's Handoffs are read, in order: its folder, then
// for a non-git folder directly under a Workspace (its key is the
// Workspace's) the Workspace root.
func places(r Reader) []string {
	out := []string{r.Dir}
	if r.Dir == "" {
		return out
	}
	home, ok := project.WorkspaceHome(r.Dir, r.Workspaces)
	if !ok || home != r.ProjectKey {
		return out
	}
	dir := filepath.Clean(r.Dir)
	for _, w := range r.Workspaces {
		w = filepath.Clean(w)
		if dir == w || filepath.Dir(dir) != w {
			continue
		}
		if h, ok := project.WorkspaceHome(w, []string{w}); ok && h == home {
			return append(out, w)
		}
	}
	return out
}
