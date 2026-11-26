# Backstory

Memory for AI coding agents on one user's machines: what agents did and why, kept so the next session starts warm.

## Memory

**Timeline**:
What Backstory observed happen: commands, tool uses and their results, session git state. Only Backstory writes it; it is fact.
_Avoid_: timeline tier, events log

**Ledger**:
The permanent, append-only set of Records and the edges between them.
_Avoid_: ledger tier

**Record**:
An entry in the Ledger that an Agent or the Human wrote on purpose, with exactly one kind: Handoff, Decision, Outcome, Note or Claim.
_Avoid_: note (for records in general), memory, entry

**Handoff**:
A Record saying what is true now, what comes next and what must not be done; the newest one is where the next Session resumes.

**Decision**:
A Record of a choice made between alternatives, and why.

**Outcome**:
A Record that work got done, citing Timeline events as its evidence.

**Unproven outcome**:
An Outcome that cites no evidence; it is an assertion, not proof.
_Avoid_: claim

**Evidence**:
The Timeline events a Record cites; meaningful only on the Device where they happened.
_Avoid_: proof, receipts

**Contradiction**:
One Record flagging another as wrong, citing Evidence. Only an Agent or the Human files one; Backstory never infers it.
_Avoid_: conflict, mismatch

**Neighbour**:
A Record close in meaning to another, found by similarity rather than by an edge. Being a Neighbour asserts nothing about the other Record, unlike Supersede or Contradiction.
_Avoid_: related record, similar record

**Note**:
A Record of a fact worth keeping that is neither a Decision nor an Outcome.

**Claim**:
A Record announcing that a Session is working on something; advisory, never enforced, and it expires on its own.
_Avoid_: lock, lease

**Tier**:
Who a Record's content comes from: `human-declared`, `agent-declared` or `inferred`. Set from observed identity, never chosen by the writer.
_Avoid_: provenance level, trust level

**Draft**:
A Record whose Tier is `inferred`: Backstory's own guess, waiting to be Promoted, and allowed to expire.
_Avoid_: suggestion, candidate, proposal

**Promote**:
The Human or an Agent confirming a Draft by writing a new Record at their own Tier that informs it. The Session the Draft was inferred from cannot promote it.
_Avoid_: approve, accept, upgrade

**Supersede**:
A newer Record replacing an older one; the older one stays in the Ledger, linked to its replacement.
_Avoid_: update, modify, replace, overwrite

**Edit**:
The Human superseding a Record with a corrected, human-declared one.
_Avoid_: update, modify

**Current**:
A Record that is neither superseded nor Tombstoned.
_Avoid_: active, live, latest

## Closing the loop on a Handoff

**Resume handoff**:
The Handoff a session start offers to continue from: the newest on the reader's branch, else the newest in the Project.
_Avoid_: resume pointer, Resume line, Resume slot

**SessionStart block**:
The short text a new Session receives before its first turn: the Resume handoff, what happened since, live sessions nearby and what needs attention.
_Avoid_: summary, memory block, warm start, context

**Possibly stale**:
A flag on a Handoff when positive evidence suggests it may be out of date; a suspicion, never a verdict.
_Avoid_: stale, outdated, expired

**Affirm**:
Confirming that a possibly stale Handoff is still right, which clears its flag until new evidence arrives.
_Avoid_: still right, acknowledge

**Done**:
The Human's mark that a Handoff's work is finished and there is nothing to resume.
_Avoid_: complete, close, resolve

## Where work happens

**Project**:
A git repository (all its worktrees and subfolders included), or a non-git folder outside any Workspace.
_Avoid_: folder, directory, cwd, repo (when a non-git Project is possible)

**Workspace**:
A folder that holds many Projects. It is never a Project itself.
_Avoid_: projects folder, project group

**Work location**:
One place work is listed in This Week: a Project, a non-git folder under a Workspace, or a Workspace root.
_Avoid_: row

**Project key**:
A Project's local name on one Device; meaningless on any other.
_Avoid_: project id, project path

**Sync identity**:
The name a Project shares across Devices: its normalised git remote, or a Link.
_Avoid_: remote, project key

**Link**:
A sync identity the Human sets for a Project, overriding or standing in for its remote.
_Avoid_: alias, mapping

## Who does the work

**Session**:
One run of a Harness, as Backstory observed it live, backfilled it from history, or received it from a Peer.
_Avoid_: run, conversation, chat

**Human**:
The identity of anything that reaches Backstory through its own command line (the panel included); not proof that a person is there.
_Avoid_: user (for the identity), owner

**Harness**:
A program Backstory recognises as an agent host by its process name (Claude Code, Codex, Hermes, Pi, ...).
_Avoid_: agent (for the program), client, tool

**Agent**:
The AI working inside a Session, identified by its Harness.
_Avoid_: model, bot

**Actor**:
An optional label a subagent declares; it shares its parent's Session.
_Avoid_: subagent session

## This Week

**This Week**:
The view of the current UTC day and the six before it: Attention, Where you left off, and each day's activity per Work location.
_Avoid_: dashboard, weekly report

**Here**:
The Project the Human is in right now: the folder of the focused terminal, else the last focused one, else the most recent Project.
_Avoid_: current project, active project

**Attention**:
The This Week section listing what needs the Human, on positive evidence only.
_Avoid_: NEEDS YOU, alerts, warnings

**Where you left off**:
The This Week section with one entry per Work location active in the window, newest first.
_Avoid_: RECENT, recent projects

## Capture

**Capture**:
Backstory adding Timeline events and accepting Records. While **Paused**, it adds and accepts nothing, for every Harness at once.
_Avoid_: recording, watching, logging

**Shell capture**:
Opt-in Capture of the commands the Human types in bash.
_Avoid_: shell history

**Backfill**:
Bringing a Harness's past Sessions in from that Harness's own history files.
_Avoid_: import

## Forgetting

**Delete**:
The Human forgetting one Record: its text is scrubbed and it becomes Tombstoned.
_Avoid_: forget, remove

**Purge**:
The Human forgetting whole Sessions' Timeline events, plus the Records written in a time window.
_Avoid_: forget, wipe, clear

**Tombstoned**:
The state of a Record whose text has been forgotten; the Record and its edges remain, and it never comes back.
_Avoid_: deleted, removed

## Sync

**Device**:
One Backstory installation, with its own store and identity.
_Avoid_: computer, machine, host

**Peer**:
A Device this one is paired with for sync.
_Avoid_: remote, partner

**Sync import**:
Bringing Records and Sessions in from a Peer.
_Avoid_: import, pull
