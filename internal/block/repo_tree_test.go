package block_test

import (
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/project"
)

const (
	treeMain = "/w/vidflow"
	treeWT   = "/w/vidflow-wt"
)

// twoTreeGit is a repo with a worktree: each tree (and anything under it)
// resolves to its own toplevel, both sharing one common dir — the project
// key the store files both under.
type twoTreeGit struct{}

func (twoTreeGit) Repo(dir string) (project.Repo, bool) {
	for _, root := range []string{treeWT, treeMain} {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return project.Repo{CommonDir: treeMain + "/.git", Toplevel: root}, true
		}
	}
	return project.Repo{}, false
}

func (twoTreeGit) State(string) (project.State, bool) { return project.State{}, false }

// The SessionStart Repo line's uncommitted count is the READER's own working
// tree's last session end, never another tree of the same repo: the project
// key (the git common dir) is shared by the main checkout and every
// worktree. Both orders, so neither "newest wins" outcome can pass.
func TestRepoLineUncommittedIsTheReadersOwnTree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dirtyFirst bool
	}{{"dirty worktree then clean main", true}, {"clean main then dirty worktree", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			mustUpsertProject(t, s, testProjectKey)
			earlier := mustStartSession(t, s, "claude", treeMain, 101)
			h := mustInsertHandoff(t, s, earlier, "handoff")
			wt := mustStartSession(t, s, "claude", treeWT, 102)
			main := mustStartSession(t, s, "claude", treeMain, 103)
			two, zero := 2, 0
			dirty := func(at time.Time) {
				mustAppendEvent(t, s, wt, payload.KindSessionGitState, at, payload.SessionGitState{Branch: "feature", UncommittedCount: &two})
			}
			clean := func(at time.Time) {
				mustAppendEvent(t, s, main, payload.KindSessionGitState, at, payload.SessionGitState{Branch: "master", UncommittedCount: &zero})
			}
			if tc.dirtyFirst {
				dirty(h.TS.Add(time.Minute))
				clean(h.TS.Add(2 * time.Minute))
			} else {
				clean(h.TS.Add(time.Minute))
				dirty(h.TS.Add(2 * time.Minute))
			}

			for _, r := range []struct {
				cwd, want, notWant string
			}{
				{treeMain + "/cmd", "branch master · 0 uncommitted", "2 uncommitted"},
				{treeWT, "branch feature · 2 uncommitted", "0 uncommitted"},
			} {
				self := mustStartSession(t, s, "claude", r.cwd, 200)
				out, err := block.Render(block.Params{
					Store: s, ProcFS: fakeProcFS{}, ProjectKey: testProjectKey, SessionID: self, Harness: "claude",
					CWD: r.cwd, Git: twoTreeGit{}, Now: h.TS.Add(time.Hour),
				})
				if err != nil {
					t.Fatalf("Render: %v", err)
				}
				line := lineWith(out, "Repo:")
				if !strings.Contains(line, r.want) || strings.Contains(line, r.notWant) {
					t.Errorf("reader in %s: Repo line = %q, want %q", r.cwd, line, r.want)
				}
			}
		})
	}
}

// A could-not-observe session end carries no evidence either way, so the Repo
// line skips it and shows the reader's tree's newest OBSERVED session-end
// state with its age, the rule This Week's uncommitted items follow (#4, the
// Human's product rule in #34). Before, a later could-not-observe end hid the
// observed state: the line showed no branch and no count.
func TestRepoLineSkipsACouldNotObserveSessionEnd(t *testing.T) {
	s := newTestStore(t)
	mustUpsertProject(t, s, testProjectKey)
	earlier := mustStartSession(t, s, "claude", treeWT, 101)
	h := mustInsertHandoff(t, s, earlier, "handoff")
	two := 2
	mustAppendEvent(t, s, earlier, payload.KindSessionGitState, h.TS.Add(time.Minute), payload.SessionGitState{Branch: "feature", UncommittedCount: &two})
	blind := mustStartSession(t, s, "claude", treeWT, 102)
	mustAppendEvent(t, s, blind, payload.KindSessionGitState, h.TS.Add(2*time.Minute), payload.SessionGitState{CouldNotObserve: true})

	self := mustStartSession(t, s, "claude", treeWT, 200)
	out, err := block.Render(block.Params{
		Store: s, ProcFS: fakeProcFS{}, ProjectKey: testProjectKey, SessionID: self, Harness: "claude",
		CWD: treeWT, Git: twoTreeGit{}, Now: h.TS.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, want := lineWith(out, "Repo:"), "Repo: branch feature · 2 uncommitted at last session end (59m ago)"; got != want {
		t.Errorf("Repo line = %q, want %q; block:\n%s", got, want, out)
	}
}
