package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/project"
)

const labelsWorkspaceDir = "/tmp/fixture/projects"

var labelsWorkspaces = []string{labelsWorkspaceDir}

// labelsFakeGit is a canned dir -> Repo lookup for sessionLabels/
// Handoffs tests: only the repo dirs a test explicitly seeds report
// as git working trees, exactly like a real filesystem where every other
// dir under the workspace is a plain, non-git folder.
type labelsFakeGit map[string]project.Repo

func (f labelsFakeGit) Repo(dir string) (project.Repo, bool) { r, ok := f[dir]; return r, ok }
func (f labelsFakeGit) State(string) (project.State, bool)   { return project.State{}, false }

var labelsGit = labelsFakeGit{
	labelsWorkspaceDir + "/omarcade": {Toplevel: labelsWorkspaceDir + "/omarcade"},
	labelsWorkspaceDir + "/vidflow":  {Toplevel: labelsWorkspaceDir + "/vidflow"},
}

func mustAppendMutatingToolUse(t *testing.T, s *Store, sessionID, name, path string) {
	t.Helper()
	b, err := json.Marshal(payload.ToolUse{Name: name, Path: path})
	if err != nil {
		t.Fatalf("marshal tool.use payload: %v", err)
	}
	if _, err := s.AppendEvent(Event{TS: time.Now(), Kind: payload.KindToolUse, SessionID: sessionID, Source: "shell", Payload: string(b)}); err != nil {
		t.Fatalf("AppendEvent tool.use: %v", err)
	}
}

// TestSessionLabelsFromFileTouchingEvents is the punch's DONE WHEN clause 2
// (task 482b2320, decision f3fa04c7): a session started at the workspace
// root whose timeline events touch files in two repos under it has labels
// exactly {projects/omarcade, projects/vidflow} — derived from the touched
// file paths, never from about[] on any record.
//
// Mutation probe: derive labels from the handoff's about[] instead of
// events (the critic clause 6 mutation) -> RED (labels would include
// "projects/other", which about[] names but no event ever touched);
// restore -> GREEN.
func TestSessionLabelsFromFileTouchingEvents(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	mustUpsertProject(t, s, "workspace:"+labelsWorkspaceDir)
	sessionID, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: labelsWorkspaceDir, ProjectKey: "workspace:" + labelsWorkspaceDir,
		StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	mustAppendMutatingToolUse(t, s, sessionID, "Edit", labelsWorkspaceDir+"/omarcade/main.go")
	mustAppendMutatingToolUse(t, s, sessionID, "Write", labelsWorkspaceDir+"/vidflow/app.py")
	// A Read carries a path too but never changed anything: it must not
	// contribute a label (payload.IsMutatingFileTool's own rule, task
	// 393d174c, reused here).
	mustAppendMutatingToolUse(t, s, sessionID, "Read", labelsWorkspaceDir+"/elsewhere/README.md")

	// The handoff's about[] names a THIRD path this session never touched a
	// file under. If labels were ever derived from about[] instead of
	// events, "projects/other" would leak into the result.
	if _, err := s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindHandoff, Text: "handoff text",
		About: []string{labelsWorkspaceDir + "/other/file.go"}, SessionID: sessionID,
		ProjectKey: "workspace:" + labelsWorkspaceDir,
	}); err != nil {
		t.Fatalf("InsertRecord handoff: %v", err)
	}

	sess, err := s.getSession(sessionID)
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	labels, err := s.sessionLabels(sess, labelsGit, labelsWorkspaces)
	if err != nil {
		t.Fatalf("sessionLabels: %v", err)
	}

	// The session's own cwd label ("projects") joins its edited folders
	// (decision 1e53165a).
	want := []string{"projects", "projects/omarcade", "projects/vidflow"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("sessionLabels = %v, want exactly %v (never projects/other, never projects/elsewhere)", labels, want)
	}
}

// TestSessionLabelsFallsBackToOwnFolder is DONE WHEN clause 2's second half:
// a session with no file-touching events at all is labelled with its own
// folder.
func TestSessionLabelsFallsBackToOwnFolder(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	mustUpsertProject(t, s, "workspace:"+labelsWorkspaceDir)
	sessionID, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: labelsWorkspaceDir, ProjectKey: "workspace:" + labelsWorkspaceDir,
		StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// A session.start event with no path at all: not a file-touching event.
	if _, err := s.AppendEvent(Event{TS: time.Now(), Kind: payload.KindSessionStart, SessionID: sessionID, Source: "shell", Payload: `{}`}); err != nil {
		t.Fatalf("AppendEvent session.start: %v", err)
	}

	sess, err := s.getSession(sessionID)
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	labels, err := s.sessionLabels(sess, labelsGit, labelsWorkspaces)
	if err != nil {
		t.Fatalf("sessionLabels: %v", err)
	}
	if want := []string{"projects"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("sessionLabels (no file-touching events) = %v, want %v (its own folder)", labels, want)
	}
}

func tempDBPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/backstory.db"
}

// Decision 1e53165a clause 2: a session started in the non-git folder
// projects/personal that edited a file in the git repo projects/vidflow is
// labelled for BOTH, and LocationScope of each folder includes it and its
// records; a session that edited nothing still maps to its cwd only.
func TestSessionLabelsIncludeCWDAndEditedDirs(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	wsKey := "workspace:" + labelsWorkspaceDir
	mustUpsertProject(t, s, wsKey)
	personal := labelsWorkspaceDir + "/personal"
	start := func(cwd string) string {
		id, err := s.StartSession(StartSessionParams{Agent: "claude", CWD: cwd, ProjectKey: wsKey, StartedAt: time.Now(), Origin: OriginLive})
		if err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		return id
	}
	note := func(sid string) string {
		id, err := s.InsertRecord(InsertRecordParams{Identity: Identity{Kind: IdentityAgent}, Kind: KindHandoff, Text: "h", SessionID: sid, ProjectKey: wsKey})
		if err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		return id
	}
	mixed := start(personal)
	mustAppendMutatingToolUse(t, s, mixed, "Edit", labelsWorkspaceDir+"/vidflow/app.py")
	mixedRec := note(mixed)
	idle := start(personal)
	idleRec := note(idle)

	for sid, want := range map[string][]string{
		mixed: {"projects/personal", "projects/vidflow"},
		idle:  {"projects/personal"},
	} {
		sess, err := s.getSession(sid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.sessionLabels(sess, labelsGit, labelsWorkspaces)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("sessionLabels(%s) = %v, want %v", sid, got, want)
		}
	}

	for _, dir := range []string{personal, labelsWorkspaceDir + "/vidflow"} {
		ls, err := s.LocationScope(dir, labelsGit, labelsWorkspaces)
		if err != nil {
			t.Fatal(err)
		}
		if !ls.HasRecord(Record{ID: mixedRec, SessionID: mixed}) {
			t.Errorf("LocationScope(%s) lacks the mixed session's record", dir)
		}
		if wantIdle := dir == personal; ls.HasRecord(Record{ID: idleRec, SessionID: idle}) != wantIdle {
			t.Errorf("LocationScope(%s) idle session membership = %v, want %v", dir, !wantIdle, wantIdle)
		}
	}
}

// A home's work locations come from the folders and touched files of
// Sessions recorded on this Device only (#5). A Peer's Session filed under the
// Workspace's own key still counts, on the Workspace's own location, but its
// folder and the files it touched are paths on the Peer and add no location.
func TestActiveHomeLabelsTakeAPeersSessionOnlyAtTheWorkspace(t *testing.T) {
	s := mustOpen(t, tempDBPath(t))
	wsKey := "workspace:" + labelsWorkspaceDir
	mustUpsertProject(t, s, wsKey)
	start := func(cwd string) string {
		t.Helper()
		id, err := s.StartSession(StartSessionParams{
			Agent: "claude", CWD: cwd, ProjectKey: wsKey, StartedAt: time.Now().Add(-time.Hour), Origin: OriginLive,
		})
		if err != nil {
			t.Fatalf("StartSession(%s): %v", cwd, err)
		}
		return id
	}
	local := start(labelsWorkspaceDir + "/scratch")
	mustAppendMutatingToolUse(t, s, local, "Edit", labelsWorkspaceDir+"/omarcade/main.go")
	peer := start(labelsWorkspaceDir + "/notes")
	mustAppendMutatingToolUse(t, s, peer, "Edit", labelsWorkspaceDir+"/vidflow/app.py")
	markPeerSession(t, s, peer)

	locs, err := s.ActiveHomeLabels(wsKey, time.Now().Add(-24*time.Hour), time.Now().Add(time.Minute), labelsGit, labelsWorkspaces, nil, nil)
	if err != nil {
		t.Fatalf("ActiveHomeLabels: %v", err)
	}
	got := map[string][]string{}
	for _, loc := range locs {
		got[loc.Label] = loc.SessionIDs
	}
	want := map[string][]string{
		"projects/scratch":  {local},
		"projects/omarcade": {local},
		"projects":          {peer},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locations (label -> sessions) = %v, want %v", got, want)
	}
	for _, loc := range locs {
		if loc.Label == "projects" && (loc.Dir != labelsWorkspaceDir || !reflect.DeepEqual(loc.CWDSessionIDs, []string{peer})) {
			t.Errorf("workspace location = %+v, want dir %s with the Peer's Session attributed whole", loc, labelsWorkspaceDir)
		}
	}
}
