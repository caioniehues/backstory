package project

import "path/filepath"

// HandoffHome is the key a Handoff is filed under when a Session in folder
// cwd, whose own key is ownKey, writes it: the one rule for a Handoff's home
// (decision f3fa04c7, amended by dc0a4387; #5). Every writer, reader and
// migration of a Handoff's key goes through it.
//
//   - ownKey itself when it names a git repo (or a Peer's placeholder for
//     one), wherever the repo lives: a repo under a Workspace keeps its
//     Handoff, so it syncs and counts on the repo's row.
//   - Otherwise the Workspace covering cwd, at any depth (WorkspaceHome): a
//     Session with no repo of its own, at the Workspace root or in a non-git
//     folder under it, homes at the Workspace.
//   - Otherwise ownKey: a non-git Project outside every Workspace, or a cwd
//     of "".
//
// Key spells a non-git folder outside a Workspace's first level as the bare
// folder, so an ownKey equal to cwd (both cleaned) is not a repo.
func HandoffHome(cwd, ownKey string, workspaces []string) string {
	if cwd == "" {
		return ownKey
	}
	if ownKey != "" && !IsWorkspaceKey(ownKey) && filepath.Clean(ownKey) != filepath.Clean(cwd) {
		return ownKey
	}
	if home, ok := WorkspaceHome(cwd, workspaces); ok {
		return home
	}
	return ownKey
}

// InHome reports whether a Session in folder cwd, whose own key is ownKey,
// belongs to home: the Sessions whose activity a Handoff filed under home is
// judged against (Possibly stale) and whose Work locations a Workspace row
// lists. Those are the Sessions whose own key is home, and, when home is a
// Workspace, every Session recorded on this Device whose folder is in it at
// any depth, whatever its own key. peer is true for a Session recorded on a
// Peer (origin_device set): its cwd is a path on the Peer, so it joins only
// by its own key. Every Session on this Device belongs to its own
// HandoffHome.
func InHome(home, cwd, ownKey string, peer bool, workspaces []string) bool {
	if ownKey == home {
		return true
	}
	if peer {
		return false
	}
	ws, ok := WorkspaceHome(cwd, workspaces)
	return ok && ws == home
}
