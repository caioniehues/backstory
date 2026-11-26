# Fork roadmap: decisions and audit (2026-11-25)

Starting point for `/wayfinder`. It gathers what was decided in conversation after the docs were read (`GLOSSARY.md`) and the architecture audit (the HTML report was written to `/tmp` and is not kept). Each decision is listed once; the map's tickets will carry the detail.

## Decisions

### Posture
- **The fork diverges from upstream (RidgetopAi/backstory).** Small bug fixes found along the way (#4's suspected staleness bug, #5's privacy gap) are candidates to send upstream. New features stay in the fork.

### Constraints kept
- **No network of its own.** `ops/nonetwork_test.go` stays as it is. Model calls go through a Harness run headless (`claude -p`, `codex exec`); Backstory never opens a connection.

### Embeddings
- **Computed on the local GPU** (AMD Radeon RX 7800 XT, 16 GB, ROCm present).
- **Transport:** (a) run a local CLI per batch, or (b) a long-running server on a unix socket. Fall back to (c), loopback TCP such as ollama, only if research rules out both (a) and (b); (c) would mean changing the no-network test and the README.
- **Prerequisite:** the `Search` seam that build lock L9 promised does not exist. Search is three concrete FTS5 methods on `Store` (`internal/store/search.go`). Create it, with two adapters: FTS5 and vector.

### Dreaming
- It is modelled on Claude Managed Agents' dreams (`POST /v1/dreams`, research preview): it consolidates a memory store plus past sessions into new output and never modifies its input. Claude Code's auto-memory has no such pass.
- **In Backstory it is Phase 5 (Inference).** A dream writes **inferred-tier draft Records with `possibly_supersedes` edges into the Ledger**; the Human or an Agent confirms them. These pieces already exist for it: `TierInferred`, `store.IdentityInference`, the rule that a session cannot promote its own draft, and the rule that Backstory's own Sessions never feed inference.
- A consolidated `MEMORY.md` export is left for later.

### Obsidian
- **A custom Obsidian plugin is the integration.** It is desktop-only (`isDesktopOnly: true`) and a client of the `backstory` CLI (`… --json`), built the way the Quickshell panel is. **It never writes memory into vault files**, so Obsidian Sync and LiveSync (both on in `~/ObsidianVault`) have nothing to copy, and Purge has nothing to chase.
- **It has its own Ledger graph view.** Records are nodes. Edges are supersedes, informs and contradicts, plus fainter embedding-neighbour edges. This needs a new `backstory graph --json` with a written contract, as PANEL-CONTRACT.md is for the panel.
- **What it can do:** read, plus Affirm, Done, Edit and Dismiss. **Not** Delete or Purge: forgetting stays in the CLI and the panel.
- **Later:** live ```backstory``` blocks in your own notes (only the query is saved; text is fetched at display time).
- **Later, optional:** a file export into a vault that does not sync, for Obsidian's own graph and Bases. It would be a second adapter at the `export` seam. Writing into a synced vault would be a warned opt-in, and Purge and Delete must reach it.
- **Install:** `backstory install obsidian --vault <path>` puts the plugin in `<vault>/.obsidian/plugins/backstory/`, with `--check` and `--remove`, and keeps files the user has edited.

### Graph
- **Do now:** the plugin's graph view, and graph-aware `recall` that follows edges and embedding neighbours.
- **Later:** a knowledge graph of entities extracted by dreaming.
- **No** external graph database: it would break the single SQLite file and the no-network rule.

## Architecture audit: deepening candidates

The scope was the most-changed areas of the last ~400 commits. "Suspected" bugs come from reading the code and are unconfirmed; each needs a failing test first.

| # | Candidate | Strength | Evidence |
|---|---|---|---|
| 1 | **Handoff status** (Done, Possibly stale, Evidence) in one module | Strong | block, week and recall each call `HandoffDone` then `HandoffFreshness` and render reasons four ways. The cleared-failure rule is only in `internal/block`, so the surfaces disagree; a3f2f72 defers the fix. |
| 2 | **Resume handoff resolver** | Strong | block's `pickResume` and week's `projectHandoff` differ (supersede, a Peer's cwd). Six shallow `LatestHandoffAt*`/`LatestRecord*` entry points. ≥9 fixes (d85c938, f43f6ae, …). |
| 3 | **Latest git state per working tree** | Strong | block's `repoStateLine` and week's `uncommittedSessionEndItems` disagree on could-not-observe. 799a232f and the git cache were each fixed twice. Product question: should the block skip could-not-observe? |
| 4 | **Handoff home rule** | Strong | `handoffHomeKey` (mcp/note.go), `sessionHomeKey` (store/labels.go) and rehome SQL have diverged. Suspected: a repo under a Workspace never goes Possibly stale from later edits. |
| 5 | **Forgetting module** (Purge plus the backup set) | Strong | Backup filenames are listed in the store's writers, `preCopyGlob` and main's `preCopyInfixes`, and d1c5c12 was a hand patch. The next pre-copy would survive a Purge. Must also cover the Obsidian file export if it is built. |
| 6 | **Capture normaliser per Harness** (live hook and Backfill) | Strong | hook.go "mirrors" backfill/claude. For Codex they diverge: live `apply_patch` yields paths and backfilled yields none; `Bash` vs `shell`. |
| 7 | **Panel: one CLI-run module** | Strong | About 9 Process sites reimplement not-installed, error-line and busy handling. Fixes came in pairs (c023f38+81146be, cc3fc80→8f0a07e). |
| 8 | **Panel: This Week view model** | Strong | `model.js` is about 40 `return p.field` accessors, and the contract scan misses fields (`outcome_id`). HereCard shows "Next:" on a Done Handoff. Suspected: week activity strips sum Workspace siblings. |
| 9 | `this-week --json` names resolved Work locations | Worth exploring | Here and Attention rows are re-resolved in JS. `attentionLabel` parses prose. Update can open the wrong Workspace sibling. |
| 10 | install organised around owned-slot kinds | Worth exploring | Eight v1.3.3–1.3.4 commits each touched 4–10 files for "ours vs the owner's". |
| 11 | One "folder on this Device" resolver | Worth exploring | About 10 near-identical getters; `recall/gitstamp.go` can use a Peer's cwd. |
| 12 | One Continue plan (panel) | Worth exploring | Only the focus step is pure; the label is decided twice. |
| – | Shared Timeline counting; one daemon-client module | Speculative | |

## Sequencing (settled in [#34](https://github.com/caioniehues/backstory/issues/34#issuecomment-7180799426))

Numbers here are GitHub issue numbers; the audit table above numbers its rows one lower.

1. **Search and embeddings (#37):** built. It merges without waiting for the forgetting module (#6), because vectors live inside the DB file and are deleted with the Record.
2. **Refactors, run as agent work alongside the grillings:**
   - **Upstream-bug checks first:** #5's staleness bug and #6's Purge pre-copy gap each start with a failing test, on a branch from `upstream/main`.
   - **One chain:** #2, then #3, then #5, then #4, because all four touch block, week and recall. #4's rule: the block skips a could-not-observe state, as week does.
   - **In parallel with the chain:** #6 and #7.
3. **Graph-aware recall:** #32 and #29 grilled together, then the PRD (#33). Gated by #2.
4. **The Obsidian plugin and graph view:** #28, then #30, then the PRD (#31). The optional file export is gated by #6.
5. **Dreaming (Phase 5):** #23, #24 and #25, then #35, then the PRD (#26). Gated by #2, #3 and #5.
6. **Lower priority, gating nothing:** #8–#13, and #1 once #36 lands.

Each build overlaps with grilling the next capability.

## Open questions for the map

- Which local embedding runtime and model; does it run on gfx1101 (ROCm or Vulkan); does it offer a per-batch CLI or a unix socket.
- Where dreams run (a timer like the sync timer? on demand?), which Harness runs them headless, and their cost and rate limits.
- Whether the block should skip could-not-observe git state (#3).
- The shape of `graph --json`: which nodes and edges, and how embedding neighbours appear.
- House rules for the fork: keep upstream's one-package-per-commit and mutation-probe receipts?
