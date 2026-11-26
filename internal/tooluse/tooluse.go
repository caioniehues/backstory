// Package tooluse is Capture's one normaliser per Harness: it turns one tool
// call into the tool.use payloads the Timeline records for it. Each Harness
// reports a tool call in two places, live (its PostToolUse hook, through
// `backstory hook post-tool-use`) and in its own history files (Backfill).
// Both go through the same function here, so a tool call captured live and
// the same call backfilled later record the same Path, Command and Name.
package tooluse

import (
	"encoding/json"
	"strings"

	"github.com/RidgetopAi/backstory/internal/payload"
)

// pathTools are the tool names whose input carries the file they act on:
// payload.MutatingFileTools (the one definition of "changed a file") plus
// Read, which observes a file without changing it but still gets its path
// recorded. A tool in neither this set nor Bash still gets a tool.use; it
// carries neither Path nor Command.
var pathTools = buildPathTools()

func buildPathTools() map[string]bool {
	m := map[string]bool{"Read": true}
	for name := range payload.MutatingFileTools {
		m[name] = true
	}
	return m
}

// bash is the tool name every Harness's shell command is recorded under.
const bash = "Bash"

// Claude normalises one Claude Code tool call. Its PostToolUse hook
// (tool_name, tool_input) and its transcript's tool_use block (name, input)
// carry the same name and input, so both pass them straight through.
func Claude(id, name string, input json.RawMessage) payload.ToolUse {
	p := payload.ToolUse{ToolUseID: id, Name: name}
	switch {
	case pathTools[name]:
		p.Path = stringField(input, "file_path")
	case name == bash:
		p.Command = stringField(input, "command")
	}
	return p
}

// codexApplyPatch is Codex's one tool for adding, updating and deleting
// files. Its patch text is the hook's tool_input.command and the rollout's
// custom_tool_call input.
const codexApplyPatch = "apply_patch"

// CodexHook normalises one call Codex's PostToolUse hook reports. The hook
// names every shell-like tool Bash and gives both Bash and apply_patch a
// tool_input.command. Any other tool_name (an MCP tool, or a tool this
// normaliser has not been told the input of) yields nothing: the hook never
// guesses at a shape. Backfill still brings such a call in by name.
func CodexHook(id, name string, input json.RawMessage) []payload.ToolUse {
	switch name {
	case bash, codexApplyPatch:
		return codex(id, name, stringField(input, "command"))
	}
	return nil
}

// CodexRolloutCall is one tool-call response_item of a Codex rollout:
// Type is function_call (Arguments is its JSON-encoded argument object) or
// custom_tool_call (Input is its raw freeform body).
type CodexRolloutCall struct {
	Type      string
	CallID    string
	Name      string
	Arguments string
	Input     string
}

// CodexRollout normalises one tool call Backfill reads from a Codex rollout.
// The rollout records the model's own tool rather than the hook's Bash:
// "shell" carries an argv, "exec_command" a cmd string, and apply_patch is a
// custom_tool_call whose Input is the patch. Any other call keeps its name
// and nothing else.
func CodexRollout(c CodexRolloutCall) []payload.ToolUse {
	switch {
	case c.Type == "function_call" && c.Name == "shell":
		var args struct {
			Command []string `json:"command"`
		}
		_ = json.Unmarshal([]byte(c.Arguments), &args)
		return codex(c.CallID, bash, strings.Join(args.Command, " "))
	case c.Type == "function_call" && c.Name == "exec_command":
		return codex(c.CallID, bash, stringField(json.RawMessage(c.Arguments), "cmd"))
	case c.Type == "custom_tool_call" && c.Name == codexApplyPatch:
		return codex(c.CallID, codexApplyPatch, c.Input)
	}
	return []payload.ToolUse{{ToolUseID: c.CallID, Name: c.Name}}
}

// codex is the shape both Codex forms share once decoded: a Bash call
// carries its command; an apply_patch call is one tool.use per path its
// patch names (Path makes it count as a changed file), or one with no Path
// when the patch names none.
func codex(id, name, command string) []payload.ToolUse {
	if name == bash {
		return []payload.ToolUse{{ToolUseID: id, Name: bash, Command: command}}
	}
	paths := applyPatchPaths(command)
	if len(paths) == 0 {
		return []payload.ToolUse{{ToolUseID: id, Name: name}}
	}
	out := make([]payload.ToolUse, 0, len(paths))
	for _, path := range paths {
		out = append(out, payload.ToolUse{ToolUseID: id, Name: name, Path: path})
	}
	return out
}

// applyPatchFileHeaders are the patch-text line prefixes that each open a
// file section. One loop scans all three, so dropping any one of them is a
// regression a test catches rather than one of three separate branches.
var applyPatchFileHeaders = []string{
	"*** Update File: ",
	"*** Add File: ",
	"*** Delete File: ",
}

// applyPatchPaths returns every path a patch's header lines name, in order.
// It never interprets the patch body.
func applyPatchPaths(patch string) []string {
	var paths []string
	for _, line := range strings.Split(patch, "\n") {
		for _, header := range applyPatchFileHeaders {
			if rest, ok := strings.CutPrefix(line, header); ok {
				paths = append(paths, strings.TrimSpace(rest))
				break
			}
		}
	}
	return paths
}

// piToolNames maps Pi's own tool names onto the names Claude's tools record
// under, so a Pi edit counts as a changed file exactly like a Claude Edit.
// Pi's extension applies the same renaming before the hook sees a call, so
// an already-renamed name passes through unchanged.
var piToolNames = map[string]string{
	"bash":  bash,
	"read":  "Read",
	"edit":  "Edit",
	"write": "Write",
}

// Pi normalises one Pi tool call: the extension's hook payload (renamed
// tool_name, tool_input with path copied to file_path) or a session file's
// toolCall block (Pi's own name, arguments with path).
func Pi(id, name string, args json.RawMessage) payload.ToolUse {
	if renamed, ok := piToolNames[name]; ok {
		name = renamed
	}
	p := payload.ToolUse{ToolUseID: id, Name: name}
	switch {
	case pathTools[name]:
		p.Path = stringField(args, "file_path", "path")
	case name == bash:
		p.Command = stringField(args, "command")
	}
	return p
}

// stringField returns the first of keys that input's JSON object carries as
// a non-empty string, or "" when none does or input is not an object. Never
// an error: a missing detail must not lose the tool.use itself.
func stringField(input json.RawMessage, keys ...string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
