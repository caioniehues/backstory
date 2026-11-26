package store

import (
	"testing"
	"time"
)

// TestHandoffFreshnessScopesToHomeAcrossRepos is the punch's DONE WHEN
// clause 4 (task 482b2320, decision f3fa04c7): a handoff written from a
// session started AT the workspace root, naming a file inside one of the
// workspace's repos, is flagged possibly-stale by a LATER edit to that file
// from a DIFFERENT session — one started inside the repo itself, which
// never shares the handoff's own (workspace) project_key. Freshness must
// scope its later-activity check to the handoff's HOME (every session whose
// folder resolves to it), not the handoff's own project_key literal.
//
// Mutation probe: scope HandoffFreshness back to the handoff's own
// project_key (the critic clause 6 mutation) -> RED (the omarcade session's
// edit event lives under omarcade's own project_key, never the workspace
// key, so the old scoping would see zero later events and never flag it);
// restore -> GREEN.
func TestHandoffFreshnessScopesToHomeAcrossRepos(t *testing.T) {
	workspaceDir := "/tmp/fixture-freshness/projects"
	workspaces := []string{workspaceDir}

	home := "workspace:" + workspaceDir
	omarcadeKey := workspaceDir + "/omarcade"
	fileF := omarcadeKey + "/F.go"

	s := mustOpen(t, tempDBPath(t))
	mustUpsertProject(t, s, home)
	mustUpsertProject(t, s, omarcadeKey)

	sessWorkspace, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: workspaceDir, ProjectKey: home, StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession(workspace): %v", err)
	}
	handoffID, err := s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindHandoff, Text: "handoff naming F in omarcade",
		About: []string{fileF}, SessionID: sessWorkspace, ProjectKey: home,
	})
	if err != nil {
		t.Fatalf("InsertRecord handoff: %v", err)
	}
	handoff, err := s.GetRecord(handoffID)
	if err != nil {
		t.Fatalf("GetRecord handoff: %v", err)
	}

	sessOmarcade, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: omarcadeKey, ProjectKey: omarcadeKey, StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession(omarcade): %v", err)
	}
	editID := mustAppendToolUseEvent(t, s, sessOmarcade, "Edit", fileF)

	reasons, err := s.HandoffFreshness(handoff, workspaces)
	if err != nil {
		t.Fatalf("HandoffFreshness: %v", err)
	}
	// The omarcade session also did unrecorded work in the home, so
	// later-unrecorded-work rides after later-activity in the fixed order.
	if len(reasons) != 2 || reasons[1].Kind != FreshnessLaterUnrecordedWork {
		t.Fatalf("HandoffFreshness = %+v, want later-activity then later-unrecorded-work", reasons)
	}
	if reasons[0].Kind != FreshnessLaterActivity {
		t.Fatalf("reason kind = %q, want %q", reasons[0].Kind, FreshnessLaterActivity)
	}
	if len(reasons[0].EventIDs) != 1 || reasons[0].EventIDs[0] != editID {
		t.Fatalf("reason event ids = %v, want [%d]", reasons[0].EventIDs, editID)
	}
}

// TestHandoffFreshnessRepoUnderWorkspaceSeesLaterEdit: a handoff written from
// a session inside a git repo that lives under a workspace dir keeps the
// repo's own key (decision dc0a4387: mcp's handoffHomeKey and the rehome
// migration). A LATER session in that same repo editing the file the handoff
// names must flag it possibly stale, exactly as it would for a repo outside
// every workspace.
func TestHandoffFreshnessRepoUnderWorkspaceSeesLaterEdit(t *testing.T) {
	workspaceDir := "/tmp/fixture-freshness/projects"
	workspaces := []string{workspaceDir}

	repoKey := workspaceDir + "/omarcade"
	fileF := repoKey + "/F.go"

	s := mustOpen(t, tempDBPath(t))
	mustUpsertProject(t, s, repoKey)

	sessA, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: repoKey, ProjectKey: repoKey, StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession(A): %v", err)
	}
	handoffID, err := s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindHandoff, Text: "handoff naming F",
		About: []string{fileF}, SessionID: sessA, ProjectKey: repoKey,
	})
	if err != nil {
		t.Fatalf("InsertRecord handoff: %v", err)
	}
	handoff, err := s.GetRecord(handoffID)
	if err != nil {
		t.Fatalf("GetRecord handoff: %v", err)
	}

	sessB, err := s.StartSession(StartSessionParams{
		Agent: "claude", CWD: repoKey, ProjectKey: repoKey, StartedAt: time.Now(), Origin: OriginLive,
	})
	if err != nil {
		t.Fatalf("StartSession(B): %v", err)
	}
	editID := mustAppendToolUseEvent(t, s, sessB, "Edit", fileF)

	reasons, err := s.HandoffFreshness(handoff, workspaces)
	if err != nil {
		t.Fatalf("HandoffFreshness: %v", err)
	}
	if len(reasons) != 2 || reasons[0].Kind != FreshnessLaterActivity || reasons[1].Kind != FreshnessLaterUnrecordedWork {
		t.Fatalf("HandoffFreshness = %+v, want later-activity then later-unrecorded-work", reasons)
	}
	if len(reasons[0].EventIDs) != 1 || reasons[0].EventIDs[0] != editID {
		t.Fatalf("later-activity event ids = %v, want [%d]", reasons[0].EventIDs, editID)
	}
}
