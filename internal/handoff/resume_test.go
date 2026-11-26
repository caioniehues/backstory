package handoff_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RidgetopAi/backstory/internal/handoff"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
)

// ledger is a store for Resume handoff tests, its Handoffs dated by the test.
type ledger struct {
	t   *testing.T
	st  *store.Store
	now time.Time
}

func newLedger(t *testing.T) *ledger {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &ledger{t: t, st: st, now: time.Now()}
}

func (l *ledger) project(key, toplevel string) {
	l.t.Helper()
	if err := l.st.UpsertProject(store.Project{Key: key, Toplevel: toplevel, FirstSeen: l.now}); err != nil {
		l.t.Fatal(err)
	}
}

func (l *ledger) session(key, cwd string, events ...string) string {
	l.t.Helper()
	sid, err := l.st.StartSession(store.StartSessionParams{Agent: "claude", CWD: cwd, ProjectKey: key, StartedAt: l.now.Add(-time.Hour), Origin: store.OriginLive})
	if err != nil {
		l.t.Fatal(err)
	}
	for _, pl := range events {
		if _, err := l.st.AppendEvent(store.Event{TS: l.now.Add(-time.Hour), Kind: "tool.use", SessionID: sid, Source: "posttooluse", Payload: pl}); err != nil {
			l.t.Fatal(err)
		}
	}
	return sid
}

// handoff files a Handoff under key, written ago before now on branch (""
// unknown), on this Device or, with peer set, a Peer's.
func (l *ledger) handoff(key, session string, ago time.Duration, branch string, peer bool) string {
	l.t.Helper()
	id := uuid.NewString()
	var origin, br, sess any
	if peer {
		origin = "dev-laptop"
	}
	if branch != "" {
		br = branch
	}
	if session != "" {
		sess = session
	}
	if _, err := l.st.DB().Exec(`INSERT INTO records
		(id, ts, kind, tier, text, about, session_id, project_key, evidence, origin_device, git_branch)
		VALUES (?, ?, 'handoff', 'agent-declared', 'h', '[]', ?, ?, '[]', ?, ?)`,
		id, l.now.Add(-ago).UnixNano(), sess, key, origin, br); err != nil {
		l.t.Fatalf("insert handoff: %v", err)
	}
	return id
}

func (l *ledger) supersede(newer, older string) {
	l.t.Helper()
	if err := l.st.LinkEdge(newer, older, store.EdgeSupersedes, "agent"); err != nil {
		l.t.Fatal(err)
	}
}

func (l *ledger) resume(r handoff.Reader) handoff.Resume {
	l.t.Helper()
	res, err := handoff.ResumeFor(l.st, r)
	if err != nil {
		l.t.Fatalf("ResumeFor: %v", err)
	}
	return res
}

func wantPick(t *testing.T, res handoff.Resume, id string) {
	t.Helper()
	if !res.Found || res.Handoff.ID != id {
		t.Fatalf("Resume handoff = %q (found %v), want %s", res.Handoff.ID, res.Found, id)
	}
}

const key = "/proj/.git"

// With no reader branch, the Resume handoff is the Project's newest,
// whichever Device wrote it; the other Device's newest rides along as Also.
func TestResumeIsTheProjectsNewestAndAlsoTheOtherDevicesNewest(t *testing.T) {
	l := newLedger(t)
	l.project(key, "/proj")
	sess := l.session(key, "/proj")
	older := l.handoff(key, sess, 3*time.Hour, "", false)
	local := l.handoff(key, sess, 2*time.Hour, "", false)
	peer := l.handoff(key, "", time.Hour, "", true)

	res := l.resume(handoff.Reader{ProjectKey: key})
	wantPick(t, res, peer)
	if res.Also == nil || res.Also.ID != local {
		t.Errorf("Also = %v, want this Device's newest %s (never the older %s)", res.Also, local, older)
	}
	if res.OtherBranch {
		t.Error("OtherBranch set with no reader branch")
	}
}

// A Handoff on the reader's branch (or on no known branch) wins over a
// newer one on another branch; with none there, the newest is the fallback,
// marked OtherBranch.
func TestResumePrefersTheReadersBranch(t *testing.T) {
	l := newLedger(t)
	l.project(key, "/proj")
	sess := l.session(key, "/proj")
	onA := l.handoff(key, sess, 3*time.Hour, "a", false)
	onB := l.handoff(key, sess, time.Hour, "b", false)

	res := l.resume(handoff.Reader{ProjectKey: key, Branch: "a"})
	if wantPick(t, res, onA); res.OtherBranch {
		t.Error("on-branch pick marked OtherBranch")
	}
	res = l.resume(handoff.Reader{ProjectKey: key, Branch: "c"})
	if wantPick(t, res, onB); !res.OtherBranch {
		t.Error("fallback pick not marked OtherBranch")
	}
	unknown := l.handoff(key, sess, 2*time.Hour, "", false)
	wantPick(t, l.resume(handoff.Reader{ProjectKey: key, Branch: "c"}), unknown)
}

// A superseded Handoff is not Current, so it is never the Resume handoff,
// even when its ts is the newest (a Peer whose clock runs ahead), and it
// earns no Also.
func TestResumeIsNeverASupersededHandoff(t *testing.T) {
	l := newLedger(t)
	l.project(key, "/proj")
	sess := l.session(key, "/proj")
	l.handoff(key, "", 4*time.Hour, "", true)
	local := l.handoff(key, sess, 2*time.Hour, "", false)
	peer := l.handoff(key, "", time.Hour, "", true)
	l.supersede(local, peer)

	res := l.resume(handoff.Reader{ProjectKey: key})
	wantPick(t, res, local)
	if res.Also != nil {
		t.Errorf("Also = %s, want none: the Peer's newest is superseded", res.Also.ID)
	}

	// The same on one Device: the newest by ts is superseded by an older one.
	edited := l.handoff(key, sess, 30*time.Minute, "", false)
	l.supersede(local, edited)
	wantPick(t, l.resume(handoff.Reader{ProjectKey: key}), local)
}

// wsGit is a fake project.Git: repo is a git repo with no remote, every other
// folder is not one.
type wsGit struct{ repo string }

func (g wsGit) Repo(dir string) (project.Repo, bool) {
	if dir != g.repo && !strings.HasPrefix(dir, g.repo+"/") {
		return project.Repo{}, false
	}
	return project.Repo{CommonDir: g.repo + "/.git", Toplevel: g.repo}, true
}

func (wsGit) State(string) (project.State, bool) { return project.State{}, false }

// At a repo's folder under a Workspace, a Handoff a root Session wrote about
// the repo is the repo's (decision f3fa04c7's location rule).
func TestResumeAtARepoUnderAWorkspaceReadsItsLocation(t *testing.T) {
	l := newLedger(t)
	ws, repo := "/w/projects", "/w/projects/x"
	git, workspaces := wsGit{repo: repo}, []string{"/w/projects"}
	repoKey, home := project.Key(repo, git, workspaces), project.Key(ws, git, workspaces)
	l.project(repoKey, repo)
	l.project(home, ws)
	root := l.session(home, ws, `{"name":"Edit","path":"/w/projects/x/main.go"}`)
	other := l.session(home, ws, `{"name":"Edit","path":"/w/projects/y/main.go"}`)
	aboutX := l.handoff(home, root, 2*time.Hour, "", false)
	l.handoff(home, other, time.Hour, "", false)

	wantPick(t, l.resume(handoff.Reader{ProjectKey: repoKey, Dir: repo, Git: git, Workspaces: workspaces}), aboutX)
}

// A non-git folder directly under a Workspace shares the root's key: with no
// Handoff of its own it carries the root's (decision 7a2556b6).
func TestResumeAtANonGitChildCarriesTheRootsHandoff(t *testing.T) {
	l := newLedger(t)
	ws, notes := "/w/projects", "/w/projects/notes"
	git, workspaces := wsGit{repo: "/w/projects/x"}, []string{ws}
	home := project.Key(ws, git, workspaces)
	l.project(home, ws)
	rootH := l.handoff(home, l.session(home, ws), time.Hour, "", false)

	wantPick(t, l.resume(handoff.Reader{ProjectKey: home, Dir: notes, Git: git, Workspaces: workspaces}), rootH)

	own := l.handoff(home, l.session(home, notes), 2*time.Hour, "", false)
	wantPick(t, l.resume(handoff.Reader{ProjectKey: home, Dir: notes, Git: git, Workspaces: workspaces}), own)
}

// Newest is the newest Current Handoff among records, by ts then id, never
// by their order.
func TestNewestIsTheNewestCurrentHandoffAmongRecords(t *testing.T) {
	l := newLedger(t)
	l.project(key, "/proj")
	sess := l.session(key, "/proj")
	current := l.handoff(key, sess, 2*time.Hour, "", false)
	superseded := l.handoff(key, "", time.Hour, "", true)
	l.supersede(current, superseded)
	oldest := l.handoff(key, sess, 3*time.Hour, "", false)
	recs, err := l.st.RecordsByIDs([]string{oldest, superseded, current})
	if err != nil {
		t.Fatal(err)
	}

	h, ok, err := handoff.Newest(l.st, recs)
	if err != nil || !ok || h.ID != current {
		t.Fatalf("Newest = %q, %v, %v; want %s", h.ID, ok, err, current)
	}
}
