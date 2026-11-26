package block

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/shellcmd"
	"github.com/RidgetopAi/backstory/internal/store"
)

// testRun is the newest observed test-runner command.
type testRun struct {
	found   bool
	cmd     string
	exit    *int
	isError bool
	ts      time.Time
}

// testsLine renders `Tests: <command> (last exit N, <age>)` for the newest
// observed test-runner command in events (ordered by id), or "" when none
// was observed. A result with no recorded exit is never rendered as exit 0:
// it reads `last failed` when is_error, else `last ok`.
func testsLine(events []store.TimelineEvent, now time.Time) string {
	toolCmd := map[string]string{}
	var last testRun
	for _, e := range events {
		switch e.Kind {
		case payload.KindToolUse:
			var tu payload.ToolUse
			if json.Unmarshal([]byte(e.Payload), &tu) == nil && tu.ToolUseID != "" && tu.Command != "" {
				toolCmd[tu.ToolUseID] = tu.Command
			}
		case payload.KindToolResult:
			var tr payload.ToolResult
			if json.Unmarshal([]byte(e.Payload), &tr) != nil {
				continue
			}
			if cmd, ok := toolCmd[tr.ToolUseID]; ok && shellcmd.IsTest(cmd) {
				last = testRun{found: true, cmd: cmd, exit: tr.Exit, isError: tr.IsError, ts: e.TS}
			}
		case payload.KindShellCommand:
			var sc payload.ShellCommand
			if json.Unmarshal([]byte(e.Payload), &sc) == nil && shellcmd.IsTest(sc.Cmd) {
				exit := sc.Exit
				last = testRun{found: true, cmd: sc.Cmd, exit: &exit, ts: e.TS}
			}
		}
	}
	if !last.found {
		return ""
	}
	cmd := shellcmd.Head(last.cmd)
	if r := []rune(cmd); len(r) > maxFailureCmdRunes {
		cmd = string(r[:maxFailureCmdRunes]) + "…"
	}
	outcome := "last ok"
	switch {
	case last.exit != nil && *last.exit != 0:
		outcome = fmt.Sprintf("last exit %d", *last.exit)
	case last.isError:
		outcome = "last failed"
	case shellcmd.Analyse(last.cmd).Piped:
		outcome = "exit not observed (piped)"
	case shellcmd.ExitUnobserved(last.cmd):
		outcome = "exit not observed"
	case last.exit != nil:
		outcome = fmt.Sprintf("last exit %d", *last.exit)
	}
	return fmt.Sprintf("Tests: %s (%s, %s)", cmd, outcome, formatAge(now.Sub(last.ts)))
}
