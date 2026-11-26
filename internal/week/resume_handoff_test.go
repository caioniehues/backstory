package week_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/project"
	"github.com/RidgetopAi/backstory/internal/store"
	"github.com/RidgetopAi/backstory/internal/week"
)

// A Peer's clock runs ahead: its Handoff arrives with a later ts than the
// local Handoff that supersedes it. The superseded Handoff is not Current, so
// the row offers the Handoff the SessionStart block resumes (#3).
func TestThisWeekHandoffIsNotASupersededPeerHandoff(t *testing.T) {
	m := newMachine(t, "a")
	if err := m.st.PairSyncPeer("dev-laptop", "laptop", "laptop.local"); err != nil {
		t.Fatal(err)
	}
	peer := store.SyncChange{Kind: store.SyncKindRecord, Record: &store.SyncRecord{
		ID: "peer-handoff-0001", TS: time.Now().Add(time.Hour).UnixNano(), Kind: string(store.KindHandoff),
		Tier: string(store.TierAgentDeclared), Text: "peer handoff, superseded", ProjectSyncID: "github.com/acme/widgets", OriginDevice: "dev-laptop",
	}}
	if _, err := m.st.ImportSyncChanges(syncedHuman, store.SyncPeer{DeviceID: "dev-laptop"}, []store.SyncChange{peer}); err != nil {
		t.Fatal(err)
	}
	local, err := m.st.InsertRecordWithEdges(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: "local handoff resuming the peer's",
		SessionID: m.sess, ProjectKey: m.key,
	}, []store.EdgeSpec{{OtherID: "peer-handoff-0001", Type: store.EdgeSupersedes, DeclaredBy: "agent", Field: "supersedes"}})
	if err != nil {
		t.Fatal(err)
	}

	if got := weekHandoff(t, m).HandoffID; got != local {
		t.Errorf("this-week handoff = %s, want the Current local one %s", got, local)
	}
	out, err := block.Render(block.Params{Store: m.st, ProjectKey: m.key, SessionID: m.sess, Harness: "claude", Now: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(id "+local+")") {
		t.Errorf("Resume is not the local handoff %s:\n%s", local, out)
	}
}

// oneRepoGit is a fake project.Git: root is a git repo with no remote, every
// other folder is not one.
type oneRepoGit struct{ root string }

func (g oneRepoGit) Repo(dir string) (project.Repo, bool) {
	if rel, err := filepath.Rel(g.root, dir); err != nil || strings.HasPrefix(rel, "..") {
		return project.Repo{}, false
	}
	return project.Repo{CommonDir: filepath.Join(g.root, ".git"), Toplevel: g.root}, true
}

func (oneRepoGit) State(string) (project.State, bool) { return project.State{}, false }

// The repo's newest Session is a Peer's, whose cwd is a folder on the Peer.
// The row's Handoff is still resolved at the repo's folder on this Device, as
// the SessionStart block there resolves it: here a Handoff a Workspace-root
// Session wrote about the repo (#3).
func TestThisWeekHandoffResolvesAtTheLocalFolderNotAPeersCWD(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := filepath.Join(t.TempDir(), "projects")
	repo := filepath.Join(ws, "x")
	git := oneRepoGit{root: repo}
	workspaces := []string{ws}
	key := project.Key(repo, git, workspaces)
	home := project.Key(ws, git, workspaces)
	now := time.Now()
	for k, dir := range map[string]string{key: repo, home: ws} {
		if err := st.UpsertProject(store.Project{Key: k, Toplevel: dir, FirstSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	session := func(id, k, cwd string, at time.Time, tool string) string {
		t.Helper()
		sid, err := st.StartSession(store.StartSessionParams{ID: id, Agent: "claude", CWD: cwd, ProjectKey: k, StartedAt: at, Origin: store.OriginLive})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendEvent(store.Event{TS: at.Add(time.Minute), Kind: "tool.use", SessionID: sid, Source: "posttooluse", Payload: tool}); err != nil {
			t.Fatal(err)
		}
		return sid
	}
	session("local", key, repo, now.Add(-3*time.Hour), `{"name":"Read","path":"main.go"}`)
	root := session("root", home, ws, now.Add(-2*time.Hour), `{"name":"Edit","path":"`+filepath.Join(repo, "main.go")+`"}`)
	h, err := st.InsertRecord(store.InsertRecordParams{
		Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: "resume x from the root",
		SessionID: root, ProjectKey: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	peerSess := session("peer", key, "/peer/home/code/x", now.Add(-time.Hour), `{"name":"Read","path":"main.go"}`)
	if _, err := st.DB().Exec(`UPDATE sessions SET origin_device = 'dev-laptop' WHERE id = ?`, peerSess); err != nil {
		t.Fatal(err)
	}

	res, err := week.Build(week.Params{Store: st, Git: git, WorkspaceDirs: workspaces, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	var row *week.ProjectSummary
	for i := range res.WhereLeftOff {
		if res.WhereLeftOff[i].Project.ProjectKey == key {
			row = &res.WhereLeftOff[i].Project
		}
	}
	if row == nil {
		t.Fatalf("no this-week row for %s: %+v", key, res.WhereLeftOff)
	}
	if row.HandoffID != h {
		t.Errorf("this-week handoff = %q, want %s (the one Resume offers at %s)", row.HandoffID, h, repo)
	}
	out, err := block.Render(block.Params{Store: st, ProjectKey: key, CWD: repo, Git: git, WorkspaceDirs: workspaces, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(id "+h+")") {
		t.Errorf("Resume at %s is not %s:\n%s", repo, h, out)
	}
}

// branchGit is oneRepoGit with branch checked out at root.
type branchGit struct {
	oneRepoGit
	branch string
}

func (g branchGit) State(dir string) (project.State, bool) {
	if _, ok := g.Repo(dir); !ok {
		return project.State{}, false
	}
	return project.State{Branch: g.branch}, true
}

// A newer Handoff on main and an older one on the branch checked out in the
// row's folder: the row lists the branch's, the one the SessionStart block
// started there resumes (#3).
func TestThisWeekHandoffPrefersTheBranchCheckedOutInItsFolder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := filepath.Join(t.TempDir(), "x")
	git := branchGit{oneRepoGit: oneRepoGit{root: repo}, branch: "feature"}
	key := project.Key(repo, git, nil)
	now := time.Now()
	if err := st.UpsertProject(store.Project{Key: key, Toplevel: repo, FirstSeen: now}); err != nil {
		t.Fatal(err)
	}
	sess, err := st.StartSession(store.StartSessionParams{ID: "local", Agent: "claude", CWD: repo, ProjectKey: key, StartedAt: now.Add(-time.Hour), Origin: store.OriginLive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(store.Event{TS: now.Add(-time.Hour), Kind: "tool.use", SessionID: sess, Source: "shell", Payload: `{}`}); err != nil {
		t.Fatal(err)
	}
	handoff := func(text, branch string) string {
		t.Helper()
		id, err := st.InsertRecord(store.InsertRecordParams{
			Identity: store.Identity{Kind: store.IdentityAgent}, Kind: store.KindHandoff, Text: text,
			SessionID: sess, ProjectKey: key, GitBranch: branch,
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		return id
	}
	onFeature := handoff("resume feature", "feature")
	handoff("resume main", "main")

	res, err := week.Build(week.Params{Store: st, Git: git, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var row *week.ProjectSummary
	for i := range res.WhereLeftOff {
		if res.WhereLeftOff[i].Project.ProjectKey == key {
			row = &res.WhereLeftOff[i].Project
		}
	}
	if row == nil {
		t.Fatalf("no this-week row for %s: %+v", key, res.WhereLeftOff)
	}
	if row.HandoffID != onFeature {
		t.Errorf("this-week handoff = %q, want %s (the one on feature, checked out at %s)", row.HandoffID, onFeature, repo)
	}
	out, err := block.Render(block.Params{Store: st, ProjectKey: key, CWD: repo, Git: git, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(id "+onFeature+")") {
		t.Errorf("Resume at %s is not %s:\n%s", repo, onFeature, out)
	}
}
