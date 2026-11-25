# How Managed Agents dreams and headless Harness runs work

Research for issue #22 (child of the wayfinder map #14). Answers two questions: what Claude Managed Agents' **dreams** do (the reference model for Backstory's Dreaming, Phase 5), and how Backstory can drive a **headless Harness run** (`claude -p`, `codex exec`) for inference without its own runs feeding inference.

Researched 2026-11-25. Claude Code docs were checked against the installed `claude` 2.1.332 (`claude --help`). Codex is **not installed** on this machine, so every Codex fact comes from its docs and source and has not been run here.

## Answer in brief

- **A dream** is an async job. It reads one memory store plus 1 to 100 past session transcripts and writes a **separate, new memory store** (by default a copy of the input, then reorganised). The input is never changed, so the result can be reviewed and thrown away. Review is all or nothing: you inspect the output store, then attach it to future sessions or delete/archive it. The API has **no built-in cadence**. The first-party cookbook runs a nightly cron over "sessions created since the last run started", finished sessions only.
- **For Backstory** this maps closely onto what already exists. The Ledger is append-only, so a dream that only writes inferred-tier Drafts with `possibly_supersedes` edges never modifies its input. Review is per Draft (Promote, or let it expire), which is finer than CMA's whole-store swap.
- **Headless runs.** Both Harnesses can give schema-checked JSON, turn off session persistence, and take large input on stdin. Recommended shapes:
  - `BACKSTORY_OFF=1 claude -p --safe-mode --no-session-persistence --strict-mcp-config --tools "" --disallowedTools "mcp__*" --output-format json --json-schema <schema> --max-turns <n> --max-budget-usd <x> --model <m>`, with the input on stdin (10 MB cap).
  - `BACKSTORY_OFF=1 codex exec --ephemeral --ignore-user-config --sandbox read-only --skip-git-repo-check --json --output-schema <file> -o <file> -m <model> -C <empty dir> -`, with the input on stdin.
- **Keeping Backstory's own runs out of inference** takes three layers. (1) **No transcript is written**: `--no-session-persistence` or `--ephemeral`, so Backfill has nothing to import. (2) **No live session**: `BACKSTORY_OFF=1` makes Backstory's hooks exit before they reach the daemon and makes its MCP tools inert, and the MCP and hook config is also not loaded. (3) **Recognised by id if anything leaks**: Backstory records the run's harness session id (the `session_id` in Claude's JSON result, which can be pre-set with `--session-id`; Codex's `thread.started` id) and skips it in Backfill and inference.
- **Cost.** Signed in with a Claude plan, `claude -p` draws from the plan's usage limits (Anthropic paused the announced change to this on 2026-06-15). With an API key it is billed per token. Codex on a ChatGPT plan shares the plan allowance; with an API key it uses API pricing. Only Claude has a spend cap flag (`--max-budget-usd`, a client-side estimate).

Confidence: **high** for the dreams API and the Claude CLI flags (first-party docs that match the installed CLI's `--help`). **Medium** for Codex flags (first-party docs and source, not run here) and for plan-limit accounting (first-party help articles that say the policy may change). **Low/unverified**: the exact interaction of `--safe-mode` and `--tools ""` with `-p --json-schema`, which needs a one-run smoke test.

---

## 1. Managed Agents dreams (reference model)

Status: **research preview**. Beta headers are `managed-agents-2026-04-01` and `dreaming-2026-04-21`. It runs on select models (Opus/Sonnet/Fable 4.6 and later), is not ZDR-eligible, and you request access. [Dreams guide][dreams]

### What goes in

- **Exactly one `memory_store` input and exactly one `sessions` input.** The sessions input lists **1 to 100** session IDs with no duplicates, all in the same workspace. The store must not be archived. If you have only transcripts, the guide says to create an empty store first. [API reference][dreams-api], [guide][dreams]
- `model` (required). `speed` only accepts `standard`. [API reference][dreams-api]
- `instructions` (optional, 1 to 4,096 characters). It steers the synthesis "throughout the pipeline: what to read closely, what to merge or drop, and how to structure the output store". It is "a synthesis pass over the inputs, not an editor": line-level directives "generally produce no change". [guide][dreams]
- A memory store is a workspace-scoped set of path-addressed text documents, mounted as a directory in the session sandbox. Every change creates "an immutable **memory version**", which gives an audit trail. There is a 10,000-memory cap per store. [Memory guide][memory]

### What comes out

- "a new, reorganized memory store: duplicates merged, stale or contradicted entries replaced with the latest value, and new insights surfaced". [guide][dreams]
- The output store's ID appears in `outputs` shortly after the job reaches `running`, once the input has been cloned. The response also carries `usage` (input, output and cache token counts) and `session_id`, which is the underlying session running the pipeline. You can stream that session's events, and it is archived (not deleted) when the dream ends. [guide][dreams]
- Lifecycle: `pending` → `running` → `completed` | `failed` | `canceled`. On `failed` or `canceled`, "the output memory store is left as-is with whatever was written before failure". Cancel is idempotent. Archiving a dream does not touch its output store. Errors include `timeout`, `input_memory_store_too_large` and `input_session_unavailable`. [guide][dreams]

### How it avoids modifying its input

- `output_behavior` defaults to `create_new`: "Write the result to a new memory store that starts as a copy of the input memory store … The dream doesn't change the input memory store." The opt-in `update_existing` writes in place. It needs write permission (otherwise 403) and returns 409 while another in-place dream on the same store is still stopping. [API reference][dreams-api]
- "The dream itself never deletes or modifies its inputs." Deleting an input store or session mid-run makes the dream fail; it is not silently skipped. [guide][dreams]

### Cadence

- **The API defines none.** Dreams "typically take minutes to a few hours, driven by the number of input transcripts". [guide][dreams]
- The first-party cookbook recipe ("Dreaming with a dynamic workflow") shows a **nightly cron (01:00)**. Each run takes sessions "created since the last run started" (the last 24 hours on the first night). "Only finished sessions are exported: those idle or terminated at 1 AM"; a session still running is skipped and is not picked up later. "A failed night retries. The next night takes the same sessions again." [cookbook][cookbook]

### How results are presented for review

- In the managed API: "Review it with the Memory Stores API or in the Claude Console, then either: **Use it** (attach it to future sessions …) or **Discard it** (delete or archive the memory store)." So review is a **whole-store swap**. There is no per-entry accept or reject and no diff view. [guide][dreams]
- In the cookbook's nightly mode, "There's no review by default". It updates the store in place, after saving a copy and with every write recorded as an auditable memory version. For review first, run into a new store and switch over after reading it. The cookbook's analyzers get read/write/edit/glob/grep only, with no shell or web access, and transcripts are mounted read-only. [cookbook][cookbook]

### Billing and limits

- "billed at standard API token rates for the model you select … Cost scales roughly linearly with the number and length of input sessions. Start with a small batch". Default rate limits apply to dream creation during the preview. [guide][dreams]

### What this means for Backstory's Dreaming

| CMA dream | Backstory equivalent (existing pieces) |
|---|---|
| Input memory store | Current Records of a Project (the Ledger) |
| 1–100 input sessions | Finished Sessions' Timeline events (and transcripts) since the last dream, **excluding Backstory's own runs** |
| New output store (`create_new`) | inferred-tier **Drafts** (`TierInferred`, `store.IdentityInference`) with `possibly_supersedes` edges. The append-only Ledger means input is never modified, with no copy needed |
| `update_existing` | **Do not offer.** Supersede and Edit stay with the Human and Agents |
| Whole-store use or discard | Per-Draft Promote. Unpromoted Drafts expire (`expires_at`, SCHEMA.md) |
| `instructions` | A fixed prompt shipped with Backstory. The Human may not change it through an Agent (AGENT-CONTRACT.md never-list, item 5: settings) |
| Partial output kept on failure | Better: write a run's Drafts in **one transaction** at the end, so a failed run leaves nothing |
| Dream's own `session_id`, archived | The headless run is itself a Session. It must be recognised and excluded (section 3) |
| Nightly, sessions since the last run, finished only, retry same window | A good default: a timer like the sync timer, plus an on-demand command. The window is "finished Sessions since the last successful dream started" |

Open points for the Dreaming PRD, not settled here:

- The rule "a draft inferred from session S cannot be promoted by session S" assumes a single source Session. A dream draws on many Sessions, so it must record **all** of them, for example as informs edges or a source list.
- Whether a dream's Draft may also be a `possibly_supersedes` of a human-declared Record, or only of agent-declared ones.

---

## 2. Headless runs

### Claude Code: `claude -p`

All from the [CLI reference][cc-cli] and [headless guide][cc-headless]. Every flag below appears in the installed 2.1.332 `--help`.

**Structured output**
- `--output-format json` returns one JSON object with `result`, `session_id`, `usage`, `total_cost_usd` and a per-model cost breakdown. `stream-json` emits JSONL events.
- `--json-schema '<schema>'` (print mode only) puts validated output in `structured_output`. "If a run ends without valid structured output, for example because Claude declined the request, the response has no `structured_output` field. Before your script uses `structured_output`, check that the field is present." An invalid schema exits with an error. `format` is treated as an annotation only.

**Restricting tools and context**
- `--tools ""` disables all built-in tools (or `--tools "Read"` allows a list). "The flag doesn't affect MCP tools; to deny those too, use `--disallowedTools "mcp__*"`."
- `--disallowedTools`: a bare name removes the tool from context, `"*"` removes every tool and `"mcp__*"` removes every MCP tool.
- `--strict-mcp-config` (with or without `--mcp-config`) skips MCP servers from `~/.claude.json`, `.mcp.json` and plugins. This is what keeps the **Backstory MCP server** out.
- `--bare` skips hooks, skills, plugins, MCP servers, auto memory and CLAUDE.md, and is "the recommended mode for scripted and SDK calls". But "bare mode doesn't use your subscription login": it **needs `ANTHROPIC_API_KEY`** (or an `apiKeyHelper`).
- `--safe-mode` disables CLAUDE.md, skills, plugins, **hooks**, **MCP servers** and auto memory, while "Authentication, model selection, built-in tools, and permissions work normally, which differs from `--bare`". It is the plan-login equivalent of bare. *Unverified:* the docs describe it for troubleshooting and do not say whether it combines with `-p`. Smoke-test it.
- `--restricted` (in `--help`) removes code-running tools and WebFetch and ignores user, project and local settings files.
- Without `--bare`, "a `-p` session runs the hooks in a project's `.claude/settings.json` and connects the servers in its `.mcp.json`, even in a folder you've never trusted". So run the headless job in an **empty temp directory**, not in a Project.
- In a default-mode `-p` run, "a call that needs approval is denied unless something approves it". Writes are denied unless allowed, even without `--tools`.

**Persistence (the key to self-exclusion)**
- `--no-session-persistence`: "sessions are not saved to disk and cannot be resumed. Print mode only." `CLAUDE_CODE_SKIP_PROMPT_HISTORY` does the same in any mode.
- `--session-id <uuid>` lets the caller choose the session id in advance.

**Large input**
- "Piped stdin is capped at 10MB. If you exceed the cap, Claude Code exits with a clear error and a non-zero status. To work with larger inputs, write the content to a file and reference the file path in your prompt." A file needs the Read tool and a path inside the working directories. For Backstory, **chunk below 10 MB** and keep `--tools ""`.
- `--system-prompt-file` or `--append-system-prompt-file` carry fixed instructions. A `__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__` line keeps the static half cached (v2.1.275 or later).

**Cost and limits**
- `--max-budget-usd` stops the run "once estimated spend … reaches this amount". It is a client-side estimate and "Spend can pass the cap". `--max-turns` limits agentic turns. `total_cost_usd` is an estimate. [cc-cli], [cc-headless], [costs][cc-costs]
- Signed in with a plan, "claude -p and Agent SDK usage still draw from your plan's usage limits". Anthropic "paused the previously-announced changes to Claude Agent SDK usage" (2026-06-15). Max, Ultra and Team monthly API credits cover `-p` only when it runs with an API key from the linked Console org. [Help: Agent SDK with your plan][help-sdk], [Help: monthly API credits][help-credits]. The policy has moved twice in 2026, so re-check it before shipping.
- `--fallback-model` handles an overloaded or retired model.

### Codex: `codex exec`

From the [non-interactive guide][cx-exec], the [command reference][cx-ref] and the Codex source ([exec/src/cli.rs][cx-cli-rs], [exec/src/lib.rs][cx-lib-rs], [protocol.rs][cx-proto]). **Not run on this machine.**

**Structured output**
- `--json`: "stdout becomes a JSON Lines (JSONL) stream". Event types are `thread.started`, `turn.started`, `turn.completed`, `turn.failed`, `item.*` and `error`. `turn.completed` carries `usage` (`input_tokens`, `cached_input_tokens`, `output_tokens`, `reasoning_output_tokens`). [cx-exec]
- `--output-schema <FILE>` is the JSON Schema for the final response. `-o, --output-last-message <FILE>` writes the final message to a file and "still prints it to stdout". [cx-exec], [cx-cli-rs]

**Restricting tools and context**
- "By default, `codex exec` runs in a read-only sandbox." `--sandbox read-only` makes that explicit. [cx-exec]
- `--ignore-user-config`: "Do not load `$CODEX_HOME/config.toml`. Authentication still uses `CODEX_HOME`." Backstory's installer registers its **MCP server** in `~/.codex/config.toml`, and Codex's **hook trust hashes** also live there (`internal/install/codex.go`). So this flag keeps Backstory's MCP out and, by inference, leaves Backstory's Codex hooks untrusted (not verified). It also drops the user's model choice, so pass `-m`. `--ignore-rules` skips execpolicy `.rules` files. [cx-ref]
- Project config and project MCP servers load only for trusted projects. Run in an empty temp dir with `-C` and `--skip-git-repo-check` ("Codex requires commands to run inside a Git repository" otherwise). [cx-exec]

**Persistence**
- `--ephemeral`: "Run without persisting session rollout files to disk." [cx-ref], [cx-cli-rs]

**Large input**
- `codex exec -` (or no prompt argument with stdin piped) reads the whole prompt from stdin. A prompt argument plus piped stdin appends stdin as a `<stdin>…</stdin>` block. No size cap is documented. [cx-lib-rs], [cx-exec]

**Cost and limits**
- Auth reuses the saved CLI login, or `CODEX_API_KEY` for a single invocation. [cx-exec]
- ChatGPT plans: "Local messages and cloud chats share your plan's usage allowance". There are five-hour windows on Plus and Business ("Pro plans currently have no five-hour limit") and "Weekly limits may also apply". "API-key sessions use separate API pricing, not your ChatGPT allowance." The page does not say how `codex exec` counts. [Codex pricing][cx-pricing]
- There is **no spend-cap flag**. Backstory must cap by input size and count runs itself, reading token `usage` from `turn.completed`.

---

## 3. Recognising a headless run's own Session (inference exclusion)

PLAN.md requires "Backstory's own sessions never feed the next inference" (§Phase 3, Human) and "Headless-run adapter per harness (`claude -p`, `codex exec`)" (§Phase 5).

**How a Session reaches Backstory today (code read in this worktree):**
- **Live:** a Harness hook (`backstory hook session-start`, `post-tool-use`, …) or the MCP shim connects to the daemon socket. The resolver walks `/proc` from the peer pid to the first process named in `ident.KnownHarnesses` (`claude`, `codex`, …), and `startSession` mints a Session (`internal/mcp/daemon.go`). A headless `claude` spawned by the daemon would be matched as an ordinary Agent (`KindAgent`). `ident.KindInference` exists, but no code produces it ("the daemon's own inference pass, never a socket peer").
- **Backfill:** at daemon start, `runClaudeBackfillOnce` and `runCodexBackfillOnce` import `~/.claude/projects/**/*.jsonl` and `~/.codex/sessions/**/rollout-*.jsonl` (`cmd/backstory/daemon.go`). Any transcript a headless run leaves behind becomes a Session at the next daemon start.
- **Opt-outs that already exist:** `BACKSTORY_OFF`: "It is read from the process environment of `backstory mcp` and every `backstory hook`, which inherit it from the harness that spawned them." Hooks return "before reading stdin or touching the daemon", and MCP tools answer a fixed off message (`internal/mcp/server.go`, `cmd/backstory/hook.go`). `BACKSTORY_NO_SESSION` exists but is honoured only on read-only methods and is meant for the installer's probe.

**Recommended layering (each layer alone is not enough):**

1. **Leave no transcript.** `claude -p --no-session-persistence` and `codex exec --ephemeral`. Backfill then has nothing to import.
2. **Never reach the socket.** Set `BACKSTORY_OFF=1` in the child's environment. Also stop the Harness loading Backstory at all: `--safe-mode` (or `--bare` with an API key) plus `--strict-mcp-config` and `--disallowedTools "mcp__*"` for Claude, `--ignore-user-config` for Codex. Run the child in an empty temp dir.
3. **Recognise it by id if anything leaks.** Before or after the run, record the harness session id in the store as an inference run. For Claude, pre-set it with `--session-id <uuid>` or read `session_id` from the JSON result. For Codex, read the id from the `thread.started` event. Backfill and the dream's input query skip those ids. This also covers a future Harness whose "no persistence" flag turns out to be leaky.
4. **The daemon writes the Drafts itself** with `store.IdentityInference`, from the run's structured output. The headless run gets no write path into Backstory.

**Weak signals, not to be relied on alone:**
- A Codex rollout's `session_meta.source` is `"exec"` for `codex exec` (`SessionSource::Exec`, client name `codex_exec`) [cx-proto], [cx-lib-rs]. Backstory's Codex backfill already reads `source` (`internal/backfill/codex/line.go`). But a user's own `codex exec` runs carry the same value, so it cannot by itself mark a run as Backstory's.
- Claude transcripts carry an `entrypoint` field. All 25 transcripts on this machine read `"cli"`. A distinct value for `-p` runs is plausible but **undocumented and not observed here**, so do not depend on it.

---

## Caveats

- Dreams are in research preview. Field names and beta headers can change.
- Codex was not installed here. Its flags come from current docs and source; confirm them with `codex exec --help` on the target version. Backstory's own Codex notes are measured against codex-cli 0.151.
- Smoke test still needed: one `claude -p --safe-mode --no-session-persistence --strict-mcp-config --tools "" --output-format json --json-schema …` run, to confirm the flags combine, that `structured_output` arrives with no tools enabled, and that no file appears under `~/.claude/projects`. The same check applies to `codex exec --ephemeral` and `~/.codex/sessions`.
- Plan-limit accounting for `claude -p` changed during 2026 (announced, then paused). Re-check it before the Dreaming PRD fixes a cadence.

## Sources

[dreams]: https://platform.claude.com/docs/en/managed-agents/dreams
[dreams-api]: https://platform.claude.com/docs/en/api/beta/dreams/create
[memory]: https://platform.claude.com/docs/en/managed-agents/memory
[cookbook]: https://platform.claude.com/cookbook/managed-agents-cma-dreaming-with-a-dynamic-workflow
[cc-cli]: https://code.claude.com/docs/en/cli-reference
[cc-headless]: https://code.claude.com/docs/en/headless
[cc-costs]: https://code.claude.com/docs/en/costs
[help-sdk]: https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan
[help-credits]: https://support.claude.com/en/articles/17154008-monthly-api-credits-for-max-ultra-and-team-plans
[cx-exec]: https://learn.chatgpt.com/docs/non-interactive-mode
[cx-ref]: https://learn.chatgpt.com/docs/developer-commands?surface=cli
[cx-pricing]: https://learn.chatgpt.com/docs/pricing
[cx-cli-rs]: https://github.com/openai/codex/blob/main/codex-rs/exec/src/cli.rs
[cx-lib-rs]: https://github.com/openai/codex/blob/main/codex-rs/exec/src/lib.rs
[cx-proto]: https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs

- Managed Agents: [Dreams guide](https://platform.claude.com/docs/en/managed-agents/dreams), [Create a dream (API)](https://platform.claude.com/docs/en/api/beta/dreams/create), [Memory stores](https://platform.claude.com/docs/en/managed-agents/memory), [Cookbook: dreaming with a dynamic workflow](https://platform.claude.com/cookbook/managed-agents-cma-dreaming-with-a-dynamic-workflow)
- Claude Code: [CLI reference](https://code.claude.com/docs/en/cli-reference), [Run programmatically (headless)](https://code.claude.com/docs/en/headless), [Costs](https://code.claude.com/docs/en/costs), installed `claude --help` (2.1.332)
- Anthropic Help Center: [Use the Agent SDK with your Claude plan](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan), [Monthly API credits](https://support.claude.com/en/articles/17154008-monthly-api-credits-for-max-ultra-and-team-plans)
- Codex: [Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode), [Developer commands](https://learn.chatgpt.com/docs/developer-commands?surface=cli), [Pricing](https://learn.chatgpt.com/docs/pricing), source [exec/src/cli.rs](https://github.com/openai/codex/blob/main/codex-rs/exec/src/cli.rs), [exec/src/lib.rs](https://github.com/openai/codex/blob/main/codex-rs/exec/src/lib.rs), [protocol/src/protocol.rs](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs)
- Backstory code: `internal/mcp/daemon.go`, `internal/ident/identity.go`, `internal/ident/harnesses.go`, `internal/mcp/server.go`, `cmd/backstory/hook.go`, `cmd/backstory/daemon.go`, `internal/install/codex.go`, `internal/backfill/codex/line.go`; `PLAN.md` §Phase 3, §Phase 5; `SCHEMA.md`; `AGENT-CONTRACT.md`
