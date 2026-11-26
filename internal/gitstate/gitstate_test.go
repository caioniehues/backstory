package gitstate_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/gitstate"
	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

const (
	key    = "/w/vidflow/.git"
	main   = "/w/vidflow"
	wt     = "/w/vidflow-wt"
	nonGit = "/w/notes"
)

// twoTrees is a repo with a worktree: each tree (and anything under it)
// resolves to its own toplevel, both sharing key. It counts every question.
type twoTrees struct{ calls *int }

func (g twoTrees) Repo(dir string) (project.Repo, bool) {
	*g.calls++
	for _, root := range []string{wt, main} {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return project.Repo{CommonDir: key, Toplevel: root}, true
		}
	}
	return project.Repo{}, false
}

func (g twoTrees) State(string) (project.State, bool) {
	*g.calls++
	return project.State{}, false
}

type fixture struct {
	t     *testing.T
	st    *store.Store
	base  time.Time
	n     int
	calls int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertProject(store.Project{Key: key, Toplevel: main, FirstSeen: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	return &fixture{t: t, st: st, base: time.Now().Add(-time.Hour)}
}

func (f *fixture) git() project.Git { return twoTrees{calls: &f.calls} }

// end starts a Session of agent in cwd and records its session-end git state.
func (f *fixture) end(agent, cwd string, gs payload.SessionGitState) string {
	f.t.Helper()
	id, err := f.st.StartSession(store.StartSessionParams{
		Agent: agent, CWD: cwd, ProjectKey: key, StartedAt: f.base, Origin: store.OriginLive,
	})
	if err != nil {
		f.t.Fatalf("StartSession: %v", err)
	}
	b, err := json.Marshal(gs)
	if err != nil {
		f.t.Fatalf("marshal: %v", err)
	}
	f.n++
	if _, err := f.st.AppendEvent(store.Event{
		TS: f.base.Add(time.Duration(f.n) * time.Minute), Kind: payload.KindSessionGitState, SessionID: id, Source: "shell", Payload: string(b),
	}); err != nil {
		f.t.Fatalf("AppendEvent: %v", err)
	}
	return id
}

func (f *fixture) events() []store.TimelineEvent {
	f.t.Helper()
	evs, err := f.st.EventsSinceID(key, 0)
	if err != nil {
		f.t.Fatalf("EventsSinceID: %v", err)
	}
	return evs
}

func count(n int) *int { return &n }

// Each working tree's latest state is its newest OBSERVED session end: a
// later could-not-observe end, a session-start observation and a shell
// session's git state carry no session-end evidence and never replace it
// (#4). A later observed end does. A Peer's Session names no tree here.
func TestLatestIsEachTreesNewestObservedSessionEnd(t *testing.T) {
	f := newFixture(t)
	f.end("claude", main, payload.SessionGitState{Branch: "master", UncommittedCount: count(3)})
	f.end("claude", wt, payload.SessionGitState{Branch: "feature", UncommittedCount: count(2)})
	f.end("claude", main+"/cmd", payload.SessionGitState{Branch: "master", UncommittedCount: count(0)})
	f.end("claude", wt, payload.SessionGitState{CouldNotObserve: true})
	f.end("claude", wt, payload.SessionGitState{Phase: payload.GitStatePhaseStart, Branch: "feature", UncommittedCount: count(9)})
	f.end("shell", wt, payload.SessionGitState{Branch: "feature", UncommittedCount: count(7)})
	peer := f.end("claude", main, payload.SessionGitState{Branch: "master", UncommittedCount: count(5)})
	if _, err := f.st.DB().Exec(`UPDATE sessions SET origin_device = 'dev-peer' WHERE id = ?`, peer); err != nil {
		t.Fatalf("mark peer: %v", err)
	}
	f.end("claude", nonGit, payload.SessionGitState{CouldNotObserve: true})

	got, err := gitstate.Latest(f.st, f.git(), f.events())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	type tb struct {
		tree, branch string
		n            int
	}
	var have []tb
	for _, s := range got {
		have = append(have, tb{s.Tree, s.Branch, s.Uncommitted})
	}
	want := []tb{{main, "master", 0}, {wt, "feature", 2}, {"", "master", 5}}
	if len(have) != len(want) {
		t.Fatalf("Latest = %+v, want %+v", have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("Latest[%d] = %+v, want %+v", i, have[i], want[i])
		}
	}
}

// LatestIn is the reader's own tree's newest observed session end, skipping
// a later could-not-observe one and other trees' ends; with no tree, the
// newest observed one anywhere. A tree with nothing observed has none.
func TestLatestInIsTheTreesNewestObservedSessionEnd(t *testing.T) {
	f := newFixture(t)
	f.end("claude", wt, payload.SessionGitState{Branch: "feature", UncommittedCount: count(2)})
	f.end("claude", main, payload.SessionGitState{Branch: "master", UncommittedCount: count(0)})
	f.end("claude", wt, payload.SessionGitState{CouldNotObserve: true})
	f.end("claude", nonGit, payload.SessionGitState{CouldNotObserve: true})

	for _, tc := range []struct {
		tree, branch string
		n            int
		found        bool
	}{
		{wt, "feature", 2, true},
		{main, "master", 0, true},
		{"", "master", 0, true},
		{nonGit, "", 0, false},
	} {
		s, ok, err := gitstate.LatestIn(f.st, f.git(), f.events(), tc.tree)
		if err != nil {
			t.Fatalf("LatestIn(%q): %v", tc.tree, err)
		}
		if ok != tc.found || s.Branch != tc.branch || s.Uncommitted != tc.n {
			t.Errorf("LatestIn(%q) = %+v, %v; want branch %q, %d uncommitted, found %v", tc.tree, s, ok, tc.branch, tc.n, tc.found)
		}
	}
}

// Resolving each Session's tree costs one git question per distinct folder,
// not one per Session, whatever Git the caller hands in (the git cache this
// read used to rely on each caller to add, #4).
func TestLatestAsksGitOncePerFolder(t *testing.T) {
	asks := func(n int) int {
		f := newFixture(t)
		for i := 0; i < n; i++ {
			dir := main
			if i%2 == 1 {
				dir = wt
			}
			f.end("claude", dir, payload.SessionGitState{Branch: "main", UncommittedCount: count(1)})
		}
		got, err := gitstate.Latest(f.st, f.git(), f.events())
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("Latest = %+v, want one per tree", got)
		}
		return f.calls
	}
	if few, many := asks(4), asks(200); few != many || many > 2 {
		t.Errorf("git questions: %d for 4 sessions, %d for 200; want equal and at most 2", few, many)
	}
}
