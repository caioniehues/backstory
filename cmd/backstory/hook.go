package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/RidgetopAi/backstory/internal/mcp"
	"github.com/RidgetopAi/backstory/internal/payload"
	"github.com/RidgetopAi/backstory/internal/tooluse"
)

// sessionStartPayload is a Claude Code SessionStart hook's JSON payload.
// Only SessionID is used: it becomes the connection's declared "session"
// join key (socket.DeclaredFields), recorded on the session as
// harness_session_id — a join key, never identity (AGENT-CONTRACT.md
// §Observed identity). cwd, transcript_path, source and hook_event_name are
// accepted so a real harness payload decodes cleanly, even though nothing
// here reads them.
type sessionStartPayload struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`
	HookEventName  string `json:"hook_event_name"`
}

// runHook is the `backstory hook` subcommand. session-start reads a Claude
// Code SessionStart payload from stdin, asks the daemon for the rendered
// SessionStart block, and prints it to stdout. post-tool-use reads a
// PostToolUse payload from stdin and records it as a live timeline event;
// it never prints anything to stdout (a PostToolUse hook's stdout is not
// injected as context the way SessionStart's is).
//
// harnessCodex is the `--harness` flag value that switches session-start and
// post-tool-use to the Codex wire shapes (task 03e19dd4, decision 3e14db82):
// SessionStart's stdout wrapped in the hookSpecificOutput envelope Codex's
// hook contract expects instead of Claude's bare block text, and
// post-tool-use's apply_patch/Bash-only mapping instead of Claude's
// every-tool-gets-an-event default. Detected from an explicit flag, never
// guessed from the payload's own shape — the payload is attacker-controlled
// input, exactly like every other field AGENT-CONTRACT.md §Observed identity
// already refuses to trust.
const harnessCodex = "codex"

// harnessPi is the `--harness` value the Pi extension passes. It selects
// tooluse.Pi, the normaliser Pi's Backfill uses, and Pi sets CommitAsk only
// when the payload opts in (postToolUsePayload.BackstoryCommitAsk).
const harnessPi = "pi"

// A hook must never break a harness boot (AGENT-CONTRACT.md §The
// SessionStart block): every failure path here — an unknown subcommand, an
// unreadable payload, no daemon socket, a daemon error — prints nothing to
// stdout, writes at most one line to stderr, and still exits 0.
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "backstory hook: usage: backstory hook session-start|post-tool-use|post-tool-use-failure|stop [--harness codex|pi]")
		return 0
	}
	sub := args[0]
	if sub != "session-start" && sub != "post-tool-use" && sub != "post-tool-use-failure" && sub != "stop" {
		_, _ = fmt.Fprintln(stderr, "backstory hook: usage: backstory hook session-start|post-tool-use|post-tool-use-failure|stop [--harness codex|pi]")
		return 0
	}

	// BACKSTORY_OFF: this harness process opted out. Return before reading
	// stdin or touching the daemon, so nothing is injected and nothing written.
	if mcp.EnvOff(os.Getenv) {
		return 0
	}

	harness, err := parseHookHarness(args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook:", err)
		return 0
	}

	switch sub {
	case "session-start":
		return runSessionStartHook(stdin, stdout, stderr, harness)
	case "stop":
		return runStopHook()
	case "post-tool-use-failure":
		return runPostToolUseFailureHook(stdin, stderr, harness)
	default:
		return runPostToolUseHook(stdin, stdout, stderr, harness)
	}
}

// parseHookHarness reads the one flag `backstory hook` accepts after its
// subcommand: --harness, defaulting to "" (Claude's shape, unchanged). Any
// other flag or argument is a usage error rather than silently ignored, so a
// typo (`--harnes codex`) fails loudly on stderr instead of quietly falling
// back to Claude's wire shape for a Codex payload.
func parseHookHarness(args []string) (string, error) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	harness := fs.String("harness", "", "harness the payload came from (codex switches to the Codex wire shapes)")
	if err := fs.Parse(args); err != nil {
		return "", fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return *harness, nil
}

// sessionStartEnvelope is the JSON stdout shape Codex's SessionStart hook
// contract expects (docs https://learn.chatgpt.com/docs/hooks): the block
// Claude's path prints as bare text wrapped so Codex knows which hook it
// belongs to and where to splice it into context. AdditionalContext carries
// exactly the same rendered block requestBlock returns for the Claude path
// against the same store — the daemon renders one warm block per project,
// never a harness-specific one.
type sessionStartEnvelope struct {
	HookSpecificOutput sessionStartHookSpecificOutput `json:"hookSpecificOutput"`
}

type sessionStartHookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func runSessionStartHook(stdin io.Reader, stdout, stderr io.Writer, harness string) int {
	var payload sessionStartPayload
	if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: decode session-start payload:", err)
		return 0
	}

	block, err := requestBlock(payload.SessionID)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook:", err)
		return 0
	}

	if harness == harnessCodex {
		envelope := sessionStartEnvelope{HookSpecificOutput: sessionStartHookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: block,
		}}
		b, err := json.Marshal(envelope)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "backstory hook: marshal session-start envelope:", err)
			return 0
		}
		_, _ = fmt.Fprintln(stdout, string(b))
		return 0
	}

	_, _ = fmt.Fprintln(stdout, block)
	return 0
}

// requestBlock dials the daemon socket exactly like `backstory mcp` does
// and asks for the rendered SessionStart block, declaring harnessSessionID
// as the "session" join key (socket.DeclaredFields) the same way the mcp
// shim declares its own. The daemon resolver's own SO_PEERCRED + /proc walk
// still decides identity, tier and project — nothing in this request can
// change any of them, whatever session id or other field the payload
// claims.
func requestBlock(harnessSessionID string) (string, error) {
	resp, err := callDaemon(harnessSessionID, mcp.DaemonMethodBlock, nil)
	if err != nil {
		return "", err
	}
	var result mcp.BlockResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("decode block result: %w", err)
	}
	return result.Block, nil
}

// callDaemon dials the daemon socket and sends one DaemonRequest line,
// declaring harnessSessionID as the "session" join key (socket.
// DeclaredFields) exactly like `backstory mcp` and requestBlock do. The
// daemon resolver's own SO_PEERCRED + /proc walk decides identity, tier and
// project from the connection alone — nothing sent here can change any of
// them.
func callDaemon(harnessSessionID, method string, params any) (mcp.DaemonResponse, error) {
	sockPath, err := socketPath()
	if err != nil {
		return mcp.DaemonResponse{}, err
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("dial daemon socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	req := mcp.DaemonRequest{Session: harnessSessionID, Method: method}
	// The installer's hook verification sets BACKSTORY_NO_SESSION so it gets
	// an answer without minting a session or project for the build dir. The
	// daemon only honours it on read-only methods; a real agent's hook never
	// has the variable set.
	if os.Getenv(noSessionEnv) != "" {
		req.NoSession = true
	}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return mcp.DaemonResponse{}, fmt.Errorf("marshal %s params: %w", method, err)
		}
		req.Params = p
	}
	b, err := json.Marshal(req)
	if err != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("marshal %s request: %w", method, err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("write daemon request: %w", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("read daemon response: %w", err)
	}
	var resp mcp.DaemonResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("decode daemon response: %w", err)
	}
	if resp.Error != nil {
		return mcp.DaemonResponse{}, fmt.Errorf("daemon: %s", resp.Error.Message)
	}
	return resp, nil
}

// postToolUsePayload is a Claude Code PostToolUse hook's JSON payload.
// CWD, TranscriptPath and HookEventName are accepted so a real harness
// payload decodes cleanly, even though nothing here reads them: the
// connection's own SO_PEERCRED + /proc identity decides the event's
// project, never anything the payload declares (AGENT-CONTRACT.md §Observed
// identity), and ToolUseID is the join key backfill's own dedup needs, not
// an identity field.
type postToolUsePayload struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	// BackstoryCommitAsk is the explicit opt-in a caller sets when it can
	// show the post-commit ask to the model (the Pi extension's tool_result
	// handler). Claude and Codex show it by construction and need no field.
	BackstoryCommitAsk bool `json:"backstory_commit_ask"`
	// Error and IsInterrupt are the PostToolUseFailure payload's own fields:
	// Claude Code reports a failed tool call with the error text instead of
	// a tool_response.
	Error       string `json:"error"`
	IsInterrupt bool   `json:"is_interrupt"`
}

// failureExitPattern reads the exit code Claude Code prefixes a failed Bash
// call's error text with ("Exit code 1\n<output>").
var failureExitPattern = regexp.MustCompile(`^\s*Exit code (\d+)`)

// buildPostToolUseFailureParams normalises one PostToolUseFailure payload:
// the same Path/Command extraction as a successful call, IsError always set,
// Exit only when the error text states one, and the error text as the output
// the daemon redacts and excerpts exactly like a Bash result's.
func buildPostToolUseFailureParams(p postToolUsePayload, harness string) mcp.PostToolUseParams {
	params := buildPostToolUseParams(p, harness)
	params.IsError = true
	params.Interrupted = p.IsInterrupt
	params.Output = payload.ElideMiddle(p.Error, payload.ToolOutputWireMaxRunes)
	if m := failureExitPattern.FindStringSubmatch(p.Error); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			params.Exit = &n
		}
	}
	return params
}

// runPostToolUseFailureHook is `backstory hook post-tool-use-failure`: the
// failure twin of runPostToolUseHook, with the same never-fail guarantees.
// Only Claude reports failures this way; a Codex payload records nothing.
func runPostToolUseFailureHook(stdin io.Reader, stderr io.Writer, harness string) int {
	if off, err := captureOff(); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: check capture-off flag:", err)
		return 0
	} else if off || harness == harnessCodex {
		return 0
	}
	var payload postToolUsePayload
	if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: decode post-tool-use-failure payload:", err)
		return 0
	}
	if payload.ToolName == "" {
		_, _ = fmt.Fprintln(stderr, "backstory hook: post-tool-use-failure payload missing tool_name")
		return 0
	}
	params := buildPostToolUseFailureParams(payload, harness)
	if _, err := callDaemon(payload.SessionID, mcp.DaemonMethodPostToolUse, params); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook:", err)
	}
	return 0
}

// toolResponseExit is a PostToolUse payload's tool_response.exit_code field,
// present only for a Bash tool call the harness itself observed exit (the
// exact field name Claude Code's hook JSON uses). ExitCode is a pointer so a
// response that omits it is distinguishable from one that observed a real
// 0 — the SCHEMA.md rule this hook must never violate: "a writer must never
// invent 0".
type toolResponseExit struct {
	ExitCode *int `json:"exit_code"`
	// Stdout, Stderr and Interrupted are what real Claude Code sends for a
	// Bash call (measured on real transcripts: interrupted, isImage,
	// noOutputExpected, persistedOutputPath, persistedOutputSize, stderr,
	// stdout — and NO exit_code). Output/Interrupted feed the tool.result's
	// outcome (task c9ab6d28).
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Interrupted bool   `json:"interrupted"`
}

// bashOutput joins a Bash tool_response's stdout and stderr into the one
// output text the daemon redacts and excerpts, cut to the wire cap (head and
// tail kept) so the request line stays small. A response that is a bare
// JSON string (a harness that reports output as text) is taken as the
// output itself.
func bashOutput(raw json.RawMessage, tr toolResponseExit) string {
	out := tr.Stdout
	if tr.Stderr != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += tr.Stderr
	}
	if out == "" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			out = text
		}
	}
	return payload.ElideMiddle(out, payload.ToolOutputWireMaxRunes)
}

// bashToolName is the name every Harness's normaliser (internal/tooluse)
// records a shell command under: the one tool whose tool_response carries
// an exit code and output the daemon turns into a tool.result.
const bashToolName = "Bash"

// backstoryNoteToolPattern matches Backstory's own note MCP tool however the
// harness prefixes it (mcp__backstory__note for Claude, mcp_backstory_note,
// backstory.note, ...). It requires the backstory server segment, so another
// server's tool that happens to be called note never matches.
var backstoryNoteToolPattern = regexp.MustCompile(`^(?:mcp[_.:-]+)?backstory[_.:-]+note$`)

// noteRecordID extracts the created record's id from a Backstory note call's
// tool_response. The MCP result reaches a hook in one of several shapes: the
// NoteResult object itself, an MCP CallToolResult carrying it as
// structuredContent or as JSON text in a content block, a bare content-block
// array, or a JSON string. It returns "" when no id can be read (a failed
// note call carries none) — never a guess.
func noteRecordID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return noteRecordID(json.RawMessage(text))
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if id := noteRecordID(json.RawMessage(b.Text)); id != "" {
				return id
			}
		}
		return ""
	}
	var obj struct {
		ID         string          `json:"id"`
		IsError    bool            `json:"isError"`
		Structured json.RawMessage `json:"structuredContent"`
		Content    json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.IsError {
		return ""
	}
	if obj.ID != "" {
		return obj.ID
	}
	if id := noteRecordID(obj.Structured); id != "" {
		return id
	}
	return noteRecordID(obj.Content)
}

// buildPostToolUseParams normalises one Claude or Pi PostToolUse payload
// into the wire shape callDaemon sends. The tool use itself (Name, Path,
// Command) comes from the Harness's normaliser in internal/tooluse, the
// one Backfill uses for the same Harness, so a call captured live and the
// same call backfilled later agree.
func buildPostToolUseParams(p postToolUsePayload, harness string) mcp.PostToolUseParams {
	if harness == harnessPi {
		return toolUseParams(p, tooluse.Pi(p.ToolUseID, p.ToolName, p.ToolInput))
	}
	return toolUseParams(p, tooluse.Claude(p.ToolUseID, p.ToolName, p.ToolInput))
}

// toolUseParams is one normalised tool use plus what only a live
// tool_response carries: for Bash, Exit (only when the response actually
// carried exit_code), the output and the interrupted flag; for Backstory's
// own note tool, the created record's id.
func toolUseParams(p postToolUsePayload, tu payload.ToolUse) mcp.PostToolUseParams {
	params := mcp.PostToolUseParams{ToolUseID: tu.ToolUseID, ToolName: tu.Name, Path: tu.Path, Command: tu.Command}
	switch {
	case tu.Name == bashToolName:
		var tr toolResponseExit
		if len(p.ToolResponse) > 0 && json.Unmarshal(p.ToolResponse, &tr) == nil {
			params.Exit = tr.ExitCode
			params.Interrupted = tr.Interrupted
			params.Output = bashOutput(p.ToolResponse, tr)
		}
	case backstoryNoteToolPattern.MatchString(tu.Name):
		params.RecordID = noteRecordID(p.ToolResponse)
	}
	return params
}

// postToolUseEnvelope is the Claude PostToolUse stdout shape that injects
// additionalContext into the model's context without blocking or forcing a
// model turn.
type postToolUseEnvelope struct {
	HookSpecificOutput postToolUseHookSpecificOutput `json:"hookSpecificOutput"`
}

type postToolUseHookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// runPostToolUseHook is `backstory hook post-tool-use`. It records the call
// and, for Claude, Codex and an opted-in Pi, prints the daemon's post-commit outcome ask — once
// per session — as PostToolUse additionalContext (never a block decision).
// Every other path prints nothing. Every failure path — capture-off, no
// daemon socket, a decode or daemon error — writes at most one line to
// stderr and exits 0 (a hook must never break a harness boot,
// AGENT-CONTRACT.md §The SessionStart block); with the capture-off flag file
// present it returns before decoding stdin at all, so a slow or malformed
// payload can never delay it.
func runPostToolUseHook(stdin io.Reader, stdout, stderr io.Writer, harness string) int {
	if off, err := captureOff(); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: check capture-off flag:", err)
		return 0
	} else if off {
		return 0
	}

	var payload postToolUsePayload
	if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: decode post-tool-use payload:", err)
		return 0
	}
	if payload.ToolName == "" {
		_, _ = fmt.Fprintln(stderr, "backstory hook: post-tool-use payload missing tool_name")
		return 0
	}

	if harness == harnessCodex {
		return runCodexPostToolUseHook(payload, stdout, stderr)
	}

	params := buildPostToolUseParams(payload, harness)
	// The installer's verification run (BACKSTORY_NO_SESSION) must never
	// settle or print the ask. Only a caller that can show the line sets
	// CommitAsk: Pi must opt in, because its fire-and-forget path throws
	// stdout away and the ask would be settled unseen.
	params.CommitAsk = os.Getenv(noSessionEnv) == "" && (harness != harnessPi || payload.BackstoryCommitAsk)
	return callAndPrintAsk(payload.SessionID, params, stdout, stderr)
}

// callAndPrintAsk sends one PostToolUse to the daemon and prints the ask it
// returns, if any, as the PostToolUse additionalContext envelope — the only
// object printed, with no decision, reason or suppressOutput (Codex's output
// schema has additionalProperties:false). Failures write at most one stderr
// line and return 0.
func callAndPrintAsk(sessionID string, params mcp.PostToolUseParams, stdout, stderr io.Writer) int {
	resp, err := callDaemon(sessionID, mcp.DaemonMethodPostToolUse, params)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook:", err)
		return 0
	}
	var result mcp.PostToolUseResult
	if json.Unmarshal(resp.Result, &result) != nil || result.AdditionalContext == "" {
		return 0
	}
	b, err := json.Marshal(postToolUseEnvelope{HookSpecificOutput: postToolUseHookSpecificOutput{
		HookEventName: "PostToolUse", AdditionalContext: result.AdditionalContext,
	}})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backstory hook: marshal post-tool-use envelope:", err)
		return 0
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// runCodexPostToolUseHook is post-tool-use's Codex mapping (task 03e19dd4,
// decision 3e14db82). tooluse.CodexHook, the normaliser Codex's Backfill
// shares, decides the tool uses: one per path an apply_patch touches, Bash
// with its command, and nothing at all for any other tool_name (an MCP
// tool, or anything it has not been told the input of), in which case the
// daemon is never dialled. Bash goes through toolUseParams exactly like
// Claude's (Codex's tool_response carries the same exit_code field), and
// Codex honours PostToolUse additionalContext, so the post-commit ask is
// shown; the installer's verification run must not settle it.
func runCodexPostToolUseHook(p postToolUsePayload, stdout, stderr io.Writer) int {
	uses := tooluse.CodexHook(p.ToolUseID, p.ToolName, p.ToolInput)
	if len(uses) == 1 && uses[0].Name == bashToolName {
		params := toolUseParams(p, uses[0])
		params.CommitAsk = os.Getenv(noSessionEnv) == ""
		return callAndPrintAsk(p.SessionID, params, stdout, stderr)
	}
	for _, tu := range uses {
		if _, err := callDaemon(p.SessionID, mcp.DaemonMethodPostToolUse, toolUseParams(p, tu)); err != nil {
			_, _ = fmt.Fprintln(stderr, "backstory hook:", err)
			return 0
		}
	}
	return 0
}

// captureOff reports whether the capture-off flag file
// (AGENT-CONTRACT.md §User-only powers, the "omarchy toggle"-style
// precedent) is present. Any error other than the file's absence is
// reported to the caller rather than silently treated as "capture is on" —
// a permissions problem on the runtime dir should be visible, not silently
// swallowed into "record everything".
func captureOff() (bool, error) {
	path, err := captureOffPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
