package store

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/project"
)

// homeFixture starts one session per (name, cwd, project key) row and returns
// the session id for each name.
func homeFixture(t *testing.T, s *Store, rows [][3]string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, r := range rows {
		mustUpsertProject(t, s, r[2])
		id, err := s.StartSession(StartSessionParams{
			Agent: "claude", CWD: r[1], ProjectKey: r[2], StartedAt: time.Now(), Origin: OriginLive,
		})
		if err != nil {
			t.Fatalf("StartSession(%s): %v", r[0], err)
		}
		ids[r[0]] = id
	}
	return ids
}

// namesOf maps sessionsForHome's sessions back to fixture names, sorted.
func namesOf(t *testing.T, ids map[string]string, got []Session) []string {
	t.Helper()
	byID := map[string]string{}
	for name, id := range ids {
		byID[id] = name
	}
	var out []string
	for _, sess := range got {
		out = append(out, byID[sess.ID])
	}
	sort.Strings(out)
	return out
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// markPeerSession records session id as a Peer's (origin_device set).
func markPeerSession(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE sessions SET origin_device = 'dev-peer' WHERE id = ?`, id); err != nil {
		t.Fatalf("mark %s a Peer's: %v", id, err)
	}
}

// TestSessionsForHomeMembership pins which sessions belong to a home
// (project.InHome): for a session on this Device, the enclosing workspace
// when its folder is inside one, at any depth and however the path is
// spelled — and, either way, the home its own project_key names, where a repo under a workspace files its
// handoffs (decision dc0a4387). sessionsForHome may narrow what it reads from
// the database, but never this answer: every session is also in the home its
// own Handoffs are filed under (project.HandoffHome, #5). A Peer's session
// joins only the home its own key names (#5).
func TestSessionsForHomeMembership(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	ws := "/srv/ws"
	workspaces := []string{ws}
	wsKey := "workspace:" + ws
	repo := "/srv/repo/.git"

	rows := [][3]string{
		{"repo-own", "/srv/repo", repo},
		{"repo-key-but-in-ws", ws + "/repo-copy", repo},
		{"other-project", "/srv/other", "/srv/other/.git"},
		{"ws-root", ws, wsKey},
		{"ws-deep", ws + "/a/b/c", "/srv/ws/a/.git"},
		{"ws-trailing-slash", ws + "/", wsKey},
		{"ws-dot", ws + "/./d", "/srv/ws/d/.git"},
		{"ws-dotdot", "/srv/elsewhere/../ws/e", "/srv/ws/e/.git"},
		{"ws-double-slash", "/srv//ws/f", "/srv/ws/f/.git"},
		{"ws-key-outside", "/tmp/moved", wsKey},
		{"ws-lookalike", "/srv/ws-other/g", "/srv/ws-other/g/.git"},
		{"ws-deep-plain", ws + "/h/i", ws + "/h/i"},
		{"peer-in-ws-repo", ws + "/p", "/srv/ws/p/.git"},
		{"peer-ws-root", ws, wsKey},
	}
	ids := homeFixture(t, s, rows)
	peers := map[string]bool{"peer-in-ws-repo": true, "peer-ws-root": true}
	for name := range peers {
		markPeerSession(t, s, ids[name])
	}

	cases := []struct {
		home string
		want []string
	}{
		{repo, []string{"repo-key-but-in-ws", "repo-own"}},
		{wsKey, []string{"peer-ws-root", "repo-key-but-in-ws", "ws-deep", "ws-deep-plain", "ws-dot", "ws-dotdot", "ws-double-slash", "ws-key-outside", "ws-root", "ws-trailing-slash"}},
		{"/srv/ws/p/.git", []string{"peer-in-ws-repo"}},
	}
	for _, r := range rows {
		if peers[r[0]] {
			continue // a Peer files its own Handoffs
		}
		home := project.HandoffHome(r[1], r[2], workspaces)
		got, err := s.sessionsForHome(home, workspaces)
		if err != nil {
			t.Fatalf("sessionsForHome(%s): %v", home, err)
		}
		if names := namesOf(t, ids, got); !containsName(names, r[0]) {
			t.Errorf("%s is not in sessionsForHome(%s), its own Handoffs' home: got %v", r[0], home, names)
		}
	}
	for _, c := range cases {
		got, err := s.sessionsForHome(c.home, workspaces)
		if err != nil {
			t.Fatalf("sessionsForHome(%s): %v", c.home, err)
		}
		if names := namesOf(t, ids, got); !sameNames(names, c.want) {
			t.Errorf("sessionsForHome(%s) = %v; want %v", c.home, names, c.want)
		}
	}
}

// A Peer's session whose folder spells a local Workspace folder's path is not
// in that location by its folder: it joins a location only by its own key
// (#5). The same session on this Device is.
func TestLocationScopeTakesAPeersSessionOnlyByItsKey(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	ws := "/srv/ws"
	workspaces := []string{ws}
	notes := ws + "/group/notes"
	ids := homeFixture(t, s, [][3]string{
		{"local", notes, notes},
		{"peer", notes, "/peer/elsewhere"},
		{"peer-own-key", "/peer/notes", notes},
	})
	markPeerSession(t, s, ids["peer"])
	markPeerSession(t, s, ids["peer-own-key"])

	ls, err := s.LocationScope(notes, fakeSweepGit{}, workspaces)
	if err != nil {
		t.Fatalf("LocationScope: %v", err)
	}
	for name, want := range map[string]bool{"local": true, "peer": false, "peer-own-key": true} {
		if got := ls.SessionIDs[ids[name]]; got != want {
			t.Errorf("%s in the scope of %s = %v, want %v", name, notes, got, want)
		}
	}
}
