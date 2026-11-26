package project_test

import (
	"testing"

	"github.com/RidgetopAi/backstory/internal/project"
)

// homeSession is one Session's shape: the folder it runs in, the key it is
// filed under, and whether it was recorded on a Peer (origin_device set).
type homeSession struct {
	name, cwd, ownKey string
	peer              bool
}

const (
	homeWS     = "/srv/projects"
	homeWSKey  = "workspace:" + homeWS
	homeRepo   = "/srv/projects/tide/.git"
	homeOutKey = "/srv/elsewhere/.git"
	homeDeep   = homeWS + "/group/plain"
)

var homeWorkspaces = []string{homeWS}

// homeSessions covers every key shape project.Key gives, inside and outside
// a Workspace, plus the odd ones a store can hold.
var homeSessions = []homeSession{
	{"workspace root", homeWS, homeWSKey, false},
	{"non-git folder directly under the workspace", homeWS + "/scratch", homeWSKey, false},
	{"non-git folder deeper under the workspace", homeDeep, homeDeep, false},
	{"unclean spelling of a deep non-git folder", homeDeep + "/", homeDeep, false},
	{"repo under the workspace", homeWS + "/tide", homeRepo, false},
	{"subfolder of a repo under the workspace", homeWS + "/tide/src", homeRepo, false},
	{"repo outside every workspace", "/srv/elsewhere", homeOutKey, false},
	{"non-git folder outside every workspace", "/srv/plain", "/srv/plain", false},
	{"workspace-keyed session whose folder left the workspace", "/tmp/moved", homeWSKey, false},
	{"no folder", "", homeRepo, false},
	{"no key", homeDeep, "", false},
	{"a Peer's placeholder", homeWS + "/tide", "sync:github.com/x/tide", true},
	{"a Peer's session in a repo under the workspace", homeWS + "/tide", homeRepo, true},
	{"a Peer's session at the workspace root", homeWS, homeWSKey, true},
	{"a Peer's session in a deep non-git folder", homeDeep, homeDeep, true},
}

// TestHandoffHome pins where a Handoff is filed (decision f3fa04c7, amended by
// dc0a4387; #5): under the writer's own key when that key is a git repo or a
// Peer's placeholder, wherever the repo lives; otherwise under the Workspace
// covering the writer's folder, at any depth; otherwise under its own key.
func TestHandoffHome(t *testing.T) {
	want := map[string]string{
		"workspace root": homeWSKey,
		"non-git folder directly under the workspace":             homeWSKey,
		"non-git folder deeper under the workspace":               homeWSKey,
		"unclean spelling of a deep non-git folder":               homeWSKey,
		"repo under the workspace":                                homeRepo,
		"subfolder of a repo under the workspace":                 homeRepo,
		"repo outside every workspace":                            homeOutKey,
		"non-git folder outside every workspace":                  "/srv/plain",
		"workspace-keyed session whose folder left the workspace": homeWSKey,
		"no folder": homeRepo,
		"no key":    homeWSKey,
	}
	for _, s := range homeSessions {
		if s.peer {
			continue // a Peer files its own Handoffs
		}
		if got := project.HandoffHome(s.cwd, s.ownKey, homeWorkspaces); got != want[s.name] {
			t.Errorf("%s: HandoffHome(%q, %q) = %q, want %q", s.name, s.cwd, s.ownKey, got, want[s.name])
		}
	}
}

// TestEverySessionIsInItsOwnHandoffsHome: the Sessions a Handoff is judged
// against (InHome) always include every Session that would have written it
// there. A repo under a Workspace whose own later edits could not flag its
// Handoff Possibly stale is the bug #5 found.
func TestEverySessionIsInItsOwnHandoffsHome(t *testing.T) {
	for _, s := range homeSessions {
		if s.peer {
			continue // a Peer files its own Handoffs
		}
		home := project.HandoffHome(s.cwd, s.ownKey, homeWorkspaces)
		if !project.InHome(home, s.cwd, s.ownKey, false, homeWorkspaces) {
			t.Errorf("%s: not in its own Handoffs' home %q", s.name, home)
		}
	}
}

// TestInHome pins which Sessions share a home: those whose own key is the
// home, and, for a Workspace, every Session recorded on this Device whose
// folder is in it at any depth, whatever its own key (a Handoff written at the
// Workspace root goes Possibly stale from an edit in any repo under it). A
// Peer's Session joins only by its own key: its folder is a path on the Peer.
func TestInHome(t *testing.T) {
	cases := []struct {
		home string
		want []string
	}{
		{homeWSKey, []string{
			"workspace root",
			"non-git folder directly under the workspace",
			"non-git folder deeper under the workspace",
			"unclean spelling of a deep non-git folder",
			"repo under the workspace",
			"subfolder of a repo under the workspace",
			"workspace-keyed session whose folder left the workspace",
			"no key",
			"a Peer's session at the workspace root",
		}},
		{homeRepo, []string{"repo under the workspace", "subfolder of a repo under the workspace", "no folder", "a Peer's session in a repo under the workspace"}},
		{homeDeep, []string{"non-git folder deeper under the workspace", "unclean spelling of a deep non-git folder", "a Peer's session in a deep non-git folder"}},
	}
	for _, c := range cases {
		want := map[string]bool{}
		for _, n := range c.want {
			want[n] = true
		}
		for _, s := range homeSessions {
			if got := project.InHome(c.home, s.cwd, s.ownKey, s.peer, homeWorkspaces); got != want[s.name] {
				t.Errorf("InHome(%q) for %s = %v, want %v", c.home, s.name, got, want[s.name])
			}
		}
	}
}
