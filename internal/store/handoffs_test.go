package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func handoffIDs(t *testing.T, s *Store, q HandoffQuery) []string {
	t.Helper()
	var ids []string
	for r, err := range s.Handoffs(q) {
		if err != nil {
			t.Fatalf("Handoffs: %v", err)
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// insertHandoffAt files a Handoff under key with a chosen ts and origin
// device ("" for this Device), bypassing InsertRecord's ts = now.
func insertHandoffAt(t *testing.T, s *Store, sessionID, key string, ts time.Time, originDevice string) string {
	t.Helper()
	id := uuid.NewString()
	var origin any
	if originDevice != "" {
		origin = originDevice
	}
	if _, err := s.db.Exec(`INSERT INTO records
		(id, ts, kind, tier, text, about, session_id, project_key, evidence, origin_device)
		VALUES (?, ?, ?, ?, 'h', '[]', ?, ?, '[]', ?)`,
		id, tsToNanos(ts), string(KindHandoff), string(TierAgentDeclared), sessionID, key, origin); err != nil {
		t.Fatalf("insert handoff fixture: %v", err)
	}
	return id
}

// A Project's Handoffs come newest first by ts then id, never by insertion
// order (a sync inserts a Handoff when it arrives); a Tombstoned one is not
// among them.
func TestHandoffsOfAProjectNewestFirstByTS(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	mustUpsertProject(t, s, "proj-a")
	sess := mustStartSessionInProject(t, s, "proj-a")
	now := time.Now()
	newest := insertHandoffAt(t, s, sess, "proj-a", now.Add(time.Hour), "")
	older := insertHandoffAt(t, s, sess, "proj-a", now, "")
	gone := insertHandoffAt(t, s, sess, "proj-a", now.Add(2*time.Hour), "")
	if err := s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRecord(InsertRecordParams{Identity: Identity{Kind: IdentityAgent}, Kind: KindNote, Text: "a note", SessionID: sess, ProjectKey: "proj-a"}); err != nil {
		t.Fatal(err)
	}

	got := handoffIDs(t, s, HandoffQuery{ProjectKey: "proj-a"})
	if want := []string{newest, older}; !reflect.DeepEqual(got, want) {
		t.Errorf("Handoffs = %v, want %v", got, want)
	}
}

// At a folder under a Workspace, the Handoffs are the ones belonging to it:
// two Handoffs share the Workspace's home key, and only the one whose
// writing Session is labelled with the folder is the folder's.
func TestHandoffsAtAFolderAreTheOnesBelongingToIt(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	home := "workspace:" + labelsWorkspaceDir
	mustUpsertProject(t, s, home)
	write := func(cwd, text string) string {
		sess, err := s.StartSession(StartSessionParams{Agent: "claude", CWD: cwd, ProjectKey: home, StartedAt: time.Now(), Origin: OriginLive})
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.InsertRecord(InsertRecordParams{Identity: Identity{Kind: IdentityAgent}, Kind: KindHandoff, Text: text, SessionID: sess, ProjectKey: home})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	omarcade := write(labelsWorkspaceDir+"/omarcade", "omarcade handoff")
	write(labelsWorkspaceDir+"/vidflow", "vidflow handoff (newer)")

	got := handoffIDs(t, s, HandoffQuery{ProjectKey: labelsWorkspaceDir + "/omarcade", Dir: labelsWorkspaceDir + "/omarcade", Git: labelsGit, Workspaces: labelsWorkspaces})
	if want := []string{omarcade}; !reflect.DeepEqual(got, want) {
		t.Errorf("Handoffs at omarcade = %v, want %v", got, want)
	}
}

// At a Workspace root, the root's own Handoffs and the ones homed in a repo
// under it come as one newest-first sequence (decision dc0a4387).
func TestHandoffsAtAWorkspaceRootMergeTheReposUnderIt(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	home := "workspace:" + labelsWorkspaceDir
	repo := labelsWorkspaceDir + "/omarcade"
	mustUpsertProject(t, s, home)
	mustUpsertProject(t, s, repo)
	write := func(key, cwd string, ts time.Time) string {
		sess, err := s.StartSession(StartSessionParams{Agent: "claude", CWD: cwd, ProjectKey: key, StartedAt: time.Now(), Origin: OriginLive})
		if err != nil {
			t.Fatal(err)
		}
		return insertHandoffAt(t, s, sess, key, ts, "")
	}
	now := time.Now()
	rootOld := write(home, labelsWorkspaceDir, now.Add(-2*time.Hour))
	repoMid := write(repo, repo, now.Add(-time.Hour))
	rootNew := write(home, labelsWorkspaceDir, now)

	got := handoffIDs(t, s, HandoffQuery{ProjectKey: home, Dir: labelsWorkspaceDir, Git: labelsGit, Workspaces: labelsWorkspaces})
	if want := []string{rootNew, repoMid, rootOld}; !reflect.DeepEqual(got, want) {
		t.Errorf("Handoffs at the root = %v, want %v", got, want)
	}
}

// From keeps this Device's Handoffs or a Peer's; Keep drops the rest.
func TestHandoffsByOriginAndKeep(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	mustUpsertProject(t, s, "proj-a")
	sess := mustStartSessionInProject(t, s, "proj-a")
	now := time.Now()
	ids := []string{
		insertHandoffAt(t, s, sess, "proj-a", now, ""),
		insertHandoffAt(t, s, sess, "proj-a", now.Add(time.Minute), "dev-laptop"),
		insertHandoffAt(t, s, sess, "proj-a", now.Add(2*time.Minute), ""),
	}
	notLast := func(r Record) bool { return r.ID != ids[2] }

	if got, want := handoffIDs(t, s, HandoffQuery{ProjectKey: "proj-a", From: OriginLocal, Keep: notLast}), []string{ids[0]}; !reflect.DeepEqual(got, want) {
		t.Errorf("local Handoffs = %v, want %v", got, want)
	}
	if got, want := handoffIDs(t, s, HandoffQuery{ProjectKey: "proj-a", From: OriginImported}), []string{ids[1]}; !reflect.DeepEqual(got, want) {
		t.Errorf("Peer Handoffs = %v, want %v", got, want)
	}
}

// Which Handoffs a folder under a Workspace reads is not project.HandoffHome
// (#4): that rule is where a Session on this Device files a Handoff, and a
// location also holds Handoffs filed elsewhere. Pinned at both kinds of
// folder, since the straight move onto HandoffHome loses one at each:
//   - A deep non-git folder files at the Workspace, where its own Sessions'
//     Handoffs are picked out by label; a Peer files by its own key, so a
//     Peer's Handoff under the folder's own key is the folder's too.
//   - A repo files under its own key, and a Handoff a root Session wrote
//     (filed at the Workspace) after editing files in the repo is the repo's
//     too, by its label.
func TestHandoffsAtAFolderReadItsWorkspaceAndItsOwnKey(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	home := "workspace:" + labelsWorkspaceDir
	deep := labelsWorkspaceDir + "/group/notes"
	repo, repoKey := labelsWorkspaceDir+"/omarcade", labelsWorkspaceDir+"/omarcade/.git"
	git := labelsFakeGit{repo: {Toplevel: repo, CommonDir: repoKey}}
	for _, k := range []string{home, deep, repoKey, labelsWorkspaceDir + "/group/drafts"} {
		mustUpsertProject(t, s, k)
	}
	start := func(cwd, key string) string {
		t.Helper()
		id, err := s.StartSession(StartSessionParams{Agent: "claude", CWD: cwd, ProjectKey: key, StartedAt: time.Now(), Origin: OriginLive})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	now := time.Now()
	local := insertHandoffAt(t, s, start(deep, deep), home, now, "")
	elsewhere := insertHandoffAt(t, s, start(labelsWorkspaceDir+"/group/drafts", labelsWorkspaceDir+"/group/drafts"), home, now.Add(time.Minute), "")
	peerSess := start("/peer/projects/group/notes", deep)
	markPeerSession(t, s, peerSess)
	peer := insertHandoffAt(t, s, peerSess, deep, now.Add(2*time.Minute), "dev-peer")

	got := handoffIDs(t, s, HandoffQuery{ProjectKey: deep, Dir: deep, Git: git, Workspaces: labelsWorkspaces})
	if want := []string{peer, local}; !reflect.DeepEqual(got, want) {
		t.Errorf("Handoffs at %s = %v, want the Peer's %s and this Device's %s (not %s, another folder's)", deep, got, peer, local, elsewhere)
	}

	inRepo := insertHandoffAt(t, s, start(repo, repoKey), repoKey, now.Add(3*time.Minute), "")
	root := start(labelsWorkspaceDir, home)
	mustAppendMutatingToolUse(t, s, root, "Edit", repo+"/main.go")
	fromRoot := insertHandoffAt(t, s, root, home, now.Add(4*time.Minute), "")
	got = handoffIDs(t, s, HandoffQuery{ProjectKey: repoKey, Dir: repo, Git: git, Workspaces: labelsWorkspaces})
	if want := []string{fromRoot, inRepo}; !reflect.DeepEqual(got, want) {
		t.Errorf("Handoffs at %s = %v, want the root Session's %s that edited it and the repo's own %s", repo, got, fromRoot, inRepo)
	}
}
