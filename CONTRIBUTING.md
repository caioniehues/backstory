# Contributing to Backstory

House rules for this fork. It is built by Caio plus Claude Code Sessions taking
`ready-for-agent` issues; there is no board, lane loop, critic, merge actor or
nightly job. The rules are short because the gate (`make check`) encodes most of
them; what is left here is what a test cannot. The canonical map of which
upstream rules the fork keeps is
[#15's resolution](https://github.com/caioniehues/backstory/issues/15#issuecomment-7170481737).

## Workflow

1. **Run tests in the FOREGROUND.** `make check` and `make integration` run in
   your terminal, blocking, and you read the exit code. Never background a test
   run and report on it later; a green you did not watch is not a green.
2. **Commit on the branch before any full run.** Uncommitted work does not
   exist. Commit first, then run `make check` / `make integration`; if the run
   is killed, the commit survives.
3. **Every commit that touches a test carries a mutation-probe trailer.** The
   commit body includes a line of the form

       RA-MUTATION-PROBE: <what you broke> -> RED (<failing test>); restored -> GREEN

   A test that has never been seen failing is not known to guard anything.
   With no critic, the trailer is self-attested: you earn it through the `tdd`
   skill's red-to-green loop, and a `code-review` pass flags any commit that
   touches a test without one.
4. **One package per commit.** A commit touches one Go package (plus its test).
   If a change needs two packages, it is two commits.
5. **Slice work into vertical-slice issues.** Each PRD is cut into issues that
   each fit one agent Session and deliver a thin path end to end. A slice may
   span several packages, as several commits. There is no S/M sizing and no
   `touches[]` serialization.
6. **No new module without a caller.** A dependency enters `go.mod` in the same
   commit as the code that calls it. The same holds for `package.json` and any
   other manifest a new toolchain brings. No "for later".
7. **No prose rule where a test can encode it.** If you find yourself writing a
   rule into a README, a comment, or this file, first ask whether `make check`
   could enforce it. Prefer the gate; delete the prose once it does.

## Gate

`make check` = `hygiene-check` + `fmt-check` + `vet` + `lint` (golangci-lint v2)
+ `test` (`go test -race -count=1 -tags backstorytest ./...`) + `panel-qml-test`.
It must exit 0 on a clean tree and non-zero on any failure. It is the only merge
gate.

- **Offline and hermetic.** `make check` never needs a GPU, a local model, a
  Harness or the network; unit tests use fakes.
- **The real runtime is checked by hand.** Tests against a real GPU, model or
  Harness sit behind a build tag (e.g. `-tags gpu`, alongside `integration`).
  Run them yourself and paste their output into the PR.
- **New toolchains join `make check` and run offline**, as `panel-qml-test` does
  for QML. The Obsidian plugin's exact targets are settled in its PRD.
- **`hygiene-check` stays in the gate**: build-loop critic and mutation files
  must never be tracked.

## Merging

- **One branch and one PR per issue.** Caio merges after green CI on the PR and
  a `code-review` pass.
- **Commits cite the GitHub issue** (`#n`), where upstream cited board task ids.
  Issue conventions are in [docs/agents/issue-tracker.md](docs/agents/issue-tracker.md).

## Fixes meant for upstream

A fix meant for upstream (`RidgetopAi/backstory`) is built on its own branch
from `upstream/main`. It follows upstream's rules in full and touches no
fork-only code, so it can be sent as-is and then merged into the fork.
