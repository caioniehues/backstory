package tooluse

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RidgetopAi/backstory/internal/payload"
)

// raw marshals v as the JSON a Harness would hand Backstory.
func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertToolUses(t *testing.T, what string, got, want []payload.ToolUse) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %+v, want %+v", what, got, want)
	}
}

// One apply_patch call, captured live by Codex's PostToolUse hook or
// backfilled from its rollout, records the same tool.use per touched path.
func TestCodexApplyPatchRecordsEachTouchedPathLiveAndBackfilled(t *testing.T) {
	patch := "*** Begin Patch\n" +
		"*** Update File: a.go\n@@\n-old\n+new\n" +
		"*** Add File: b.go\n+package b\n" +
		"*** Delete File: c.go\n" +
		"*** End Patch\n"
	want := []payload.ToolUse{
		{ToolUseID: "call_1", Name: "apply_patch", Path: "a.go"},
		{ToolUseID: "call_1", Name: "apply_patch", Path: "b.go"},
		{ToolUseID: "call_1", Name: "apply_patch", Path: "c.go"},
	}
	assertToolUses(t, "live", CodexHook("call_1", "apply_patch", raw(t, map[string]any{"command": patch})), want)
	assertToolUses(t, "backfilled", CodexRollout(CodexRolloutCall{
		Type: "custom_tool_call", CallID: "call_1", Name: "apply_patch", Input: patch,
	}), want)
}

// A patch naming no file still records the call, live and backfilled alike.
func TestCodexApplyPatchNamingNoFileRecordsOneToolUse(t *testing.T) {
	patch := "*** Begin Patch\n*** End Patch\n"
	want := []payload.ToolUse{{ToolUseID: "call_2", Name: "apply_patch"}}
	assertToolUses(t, "live", CodexHook("call_2", "apply_patch", raw(t, map[string]any{"command": patch})), want)
	assertToolUses(t, "backfilled", CodexRollout(CodexRolloutCall{
		Type: "custom_tool_call", CallID: "call_2", Name: "apply_patch", Input: patch,
	}), want)
}

// Codex's hook names every shell-like tool Bash; its rollout records the
// model's own tool (shell with an argv, exec_command with a cmd string).
// All three are one Bash tool use carrying the command.
func TestCodexShellCommandIsBashLiveAndBackfilled(t *testing.T) {
	want := []payload.ToolUse{{ToolUseID: "call_3", Name: "Bash", Command: "ls -la"}}
	assertToolUses(t, "live", CodexHook("call_3", "Bash", raw(t, map[string]any{"command": "ls -la"})), want)
	assertToolUses(t, "backfilled shell", CodexRollout(CodexRolloutCall{
		Type: "function_call", CallID: "call_3", Name: "shell", Arguments: `{"command":["ls","-la"]}`,
	}), want)
	assertToolUses(t, "backfilled exec_command", CodexRollout(CodexRolloutCall{
		Type: "function_call", CallID: "call_3", Name: "exec_command", Arguments: `{"cmd":"ls -la"}`,
	}), want)
}

// A tool whose input Backstory has not been told the shape of: the hook
// records nothing (never a guess), Backfill keeps the call by name only.
func TestCodexOtherToolIsNotGuessedAt(t *testing.T) {
	if got := CodexHook("call_4", "mcp__fs__read", raw(t, map[string]any{"path": "x"})); got != nil {
		t.Errorf("live = %+v, want nil", got)
	}
	assertToolUses(t, "backfilled", CodexRollout(CodexRolloutCall{
		Type: "function_call", CallID: "call_4", Name: "update_plan", Arguments: `{"plan":[]}`,
	}), []payload.ToolUse{{ToolUseID: "call_4", Name: "update_plan"}})
}

// Pi's extension renames bash/read/edit/write and copies path to file_path
// before the hook sees a call; Pi's session file keeps the raw toolCall.
// Both forms of one call record the same tool use.
func TestPiToolCallIsTheSameLiveAndBackfilled(t *testing.T) {
	for _, tc := range []struct {
		piName, hookName string
		args             map[string]any
		want             payload.ToolUse
	}{
		{"edit", "Edit", map[string]any{"path": "a.go", "oldText": "x", "newText": "y"}, payload.ToolUse{ToolUseID: "c1", Name: "Edit", Path: "a.go"}},
		{"write", "Write", map[string]any{"path": "b.go", "content": "package b"}, payload.ToolUse{ToolUseID: "c1", Name: "Write", Path: "b.go"}},
		{"read", "Read", map[string]any{"path": "c.go"}, payload.ToolUse{ToolUseID: "c1", Name: "Read", Path: "c.go"}},
		{"bash", "Bash", map[string]any{"command": "go test ./..."}, payload.ToolUse{ToolUseID: "c1", Name: "Bash", Command: "go test ./..."}},
		{"grep", "grep", map[string]any{"pattern": "TODO"}, payload.ToolUse{ToolUseID: "c1", Name: "grep"}},
	} {
		t.Run(tc.piName, func(t *testing.T) {
			hookInput := map[string]any{}
			for k, v := range tc.args {
				hookInput[k] = v
			}
			if p, ok := tc.args["path"]; ok {
				hookInput["file_path"] = p
			}
			if got := Pi("c1", tc.hookName, raw(t, hookInput)); got != tc.want {
				t.Errorf("live = %+v, want %+v", got, tc.want)
			}
			if got := Pi("c1", tc.piName, raw(t, tc.args)); got != tc.want {
				t.Errorf("backfilled = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Claude Code's hook and its transcript carry the same name and input.
func TestClaudeToolUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  payload.ToolUse
	}{
		{"Edit", map[string]any{"file_path": "/p/a.go", "old_string": "x"}, payload.ToolUse{ToolUseID: "t1", Name: "Edit", Path: "/p/a.go"}},
		{"Read", map[string]any{"file_path": "/p/b.go"}, payload.ToolUse{ToolUseID: "t1", Name: "Read", Path: "/p/b.go"}},
		{"Bash", map[string]any{"command": "make check"}, payload.ToolUse{ToolUseID: "t1", Name: "Bash", Command: "make check"}},
		{"Grep", map[string]any{"pattern": "x", "path": "/p"}, payload.ToolUse{ToolUseID: "t1", Name: "Grep"}},
	} {
		if got := Claude("t1", tc.name, raw(t, tc.input)); got != tc.want {
			t.Errorf("Claude(%s) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
