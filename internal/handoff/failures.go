package handoff

import (
	"encoding/json"
	"time"

	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/shellcmd"
	"github.com/RidgetopAi/backstory/internal/store"
)

// Failure is one failure no later pass cleared.
type Failure struct {
	EventID int64
	// Command is the failed command, the tool name for a tool with no
	// command, or "(command not recorded)" when no tool.use was observed.
	Command string
	// Exit is 0 when the failure was reported without an exit code (a
	// tool.result with is_error and no exit).
	Exit int
	TS   time.Time
}

// OpenFailures returns, in event order, every failure in events (ordered by
// id) that no later pass cleared: a shell `command` event with a non-zero
// exit, or a tool.result that is_error or carries a non-zero exit (its
// command is read back from the matching tool.use). A later result for the
// same subject clears it: the same Bash command (by shellcmd.Key, so `-v` or
// a piped `| tail` does not make it a different one), or the same tool and
// path, coming back as a non-error tool.result or a shell event with exit 0,
// whose exit status is the command's own (shellcmd.ExitUnobserved). A
// different command passing says nothing about the one that failed, so it
// clears nothing; a later failure of the same subject clears nothing either.
//
// This is the cleared-failure rule: the block's Last failure line shows the
// newest open failure, and Of drops a later-failure reason's cleared ones.
func OpenFailures(events []store.TimelineEvent) []Failure {
	type subject struct{ key, cmd string }
	toolSubject := map[string]subject{}
	var open []Failure
	var keys []string
	clear := func(key string) {
		kept, keptKeys := open[:0], keys[:0]
		for i, f := range open {
			if keys[i] != key {
				kept = append(kept, f)
				keptKeys = append(keptKeys, keys[i])
			}
		}
		open, keys = kept, keptKeys
	}
	for _, e := range events {
		switch e.Kind {
		case payload.KindToolUse:
			var tu payload.ToolUse
			if json.Unmarshal([]byte(e.Payload), &tu) == nil && tu.ToolUseID != "" {
				sub := subject{cmd: tu.Command, key: "cmd:" + shellcmd.Key(tu.Command)}
				if tu.Command == "" {
					sub.cmd = tu.Name
					sub.key = "tool:" + tu.Name + ":" + tu.Path
				}
				toolSubject[tu.ToolUseID] = sub
			}
		case payload.KindToolResult:
			var tr payload.ToolResult
			if json.Unmarshal([]byte(e.Payload), &tr) != nil {
				continue
			}
			sub, known := toolSubject[tr.ToolUseID]
			failed := tr.IsError || (tr.Exit != nil && *tr.Exit != 0)
			if !failed {
				if known && !shellcmd.ExitUnobserved(sub.cmd) {
					clear(sub.key)
				}
				continue
			}
			if !known {
				sub = subject{cmd: "(command not recorded)", key: "result:" + tr.ToolUseID}
			}
			exit := 0
			if tr.Exit != nil {
				exit = *tr.Exit
			}
			open = append(open, Failure{EventID: e.ID, Command: sub.cmd, Exit: exit, TS: e.TS})
			keys = append(keys, sub.key)
		case payload.KindShellCommand:
			var sc payload.ShellCommand
			if json.Unmarshal([]byte(e.Payload), &sc) != nil {
				continue
			}
			key := "cmd:" + shellcmd.Key(sc.Cmd)
			if sc.Exit == 0 {
				if !shellcmd.ExitUnobserved(sc.Cmd) {
					clear(key)
				}
				continue
			}
			open = append(open, Failure{EventID: e.ID, Command: sc.Cmd, Exit: sc.Exit, TS: e.TS})
			keys = append(keys, key)
		}
	}
	return open
}

// dropClearedFailures removes from the later-failure reason every failure a
// later pass in the failure's own scope (store.LaterFailureScope) cleared,
// and the reason itself when none is left. Other reasons are untouched.
func dropClearedFailures(st *store.Store, h store.Record, workspaces []string, reasons []store.FreshnessReason) ([]store.FreshnessReason, error) {
	at := -1
	for i, r := range reasons {
		if r.Kind == store.FreshnessLaterFailure {
			at = i
		}
	}
	if at < 0 {
		return reasons, nil
	}
	scope, err := st.LaterFailureScope(h, workspaces)
	if err != nil {
		return nil, err
	}
	open := map[int64]bool{}
	for _, f := range OpenFailures(scope) {
		open[f.EventID] = true
	}
	r := reasons[at]
	kept := store.FreshnessReason{Kind: r.Kind}
	for i, id := range r.EventIDs {
		if open[id] {
			kept.EventIDs = append(kept.EventIDs, id)
			kept.Failures = append(kept.Failures, r.Failures[i])
		}
	}
	out := append([]store.FreshnessReason{}, reasons[:at]...)
	if len(kept.EventIDs) > 0 {
		out = append(out, kept)
	}
	return append(out, reasons[at+1:]...), nil
}
