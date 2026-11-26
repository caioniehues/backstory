package main

import (
	"reflect"
	"testing"

	"github.com/RidgetopAi/backstory/internal/payload"
)

// A Codex apply_patch whose patch names no file is still a tool call: the
// live hook records it, as Backfill does for the same call in a rollout.
func TestHookPostToolUseCodexApplyPatchNamingNoFileRecordsTheCall(t *testing.T) {
	got := hookToolUsePayloadsFor(t, []string{"post-tool-use", "--harness", "codex"}, map[string]any{
		"session_id": "codex-empty-patch", "tool_name": "apply_patch", "tool_use_id": "call_empty",
		"tool_input":    map[string]any{"command": "*** Begin Patch\n*** End Patch\n"},
		"tool_response": "Done!",
	})
	want := []payload.ToolUse{{ToolUseID: "call_empty", Name: "apply_patch"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool.use payloads = %+v, want %+v", got, want)
	}
}

// A Pi call in Pi's own names and arguments (edit with a path, as its
// session file records it) is captured live exactly as Backfill records it.
func TestHookPostToolUsePiRecordsPiToolNamesLikeBackfill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  payload.ToolUse
	}{
		{"edit", map[string]any{"path": "main.go", "oldText": "x", "newText": "y"}, payload.ToolUse{ToolUseID: "call_pi", Name: "Edit", Path: "main.go"}},
		{"bash", map[string]any{"command": "go test ./..."}, payload.ToolUse{ToolUseID: "call_pi", Name: "Bash", Command: "go test ./..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hookToolUsePayloadsFor(t, []string{"post-tool-use", "--harness", "pi"}, map[string]any{
				"session_id": "pi-raw-" + tc.name, "tool_name": tc.name, "tool_use_id": "call_pi",
				"tool_input": tc.input, "tool_response": map[string]any{"text": "ok"},
			})
			if want := []payload.ToolUse{tc.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("tool.use payloads = %+v, want %+v", got, want)
			}
		})
	}
}
