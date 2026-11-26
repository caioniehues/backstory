-- 0029_unrehome_folder_handoffs.sql — repair the pre-#5 handoff re-home (#5).
-- Before #5, the re-home at Open moved a Handoff written in a non-git folder
-- deeper under a Workspace (a Session keyed to its bare folder path) onto
-- that folder's path, as if it were a repo. This migration moves each one
-- back to the Workspace's key, where project.HandoffHome files it. The work
-- is Go-side (unrehomeFolderHandoffs, a store hook in migrate.go) because it
-- needs the configured Workspaces; a pre-copy (forgetting.Unrehome) is taken
-- first, only when there is something to move.
SELECT 1;
