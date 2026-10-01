# 0005: Keeping other people's code off the machine

**Status: built and tested against fakes; open on the real runner and GitHub.** The behavior
this depends on is listed below and has not been observed.

## Decision

Enforcement is a **job-started hook on the machine**, not a guard in the workflow.

The first design put the guard in the workflow (`if: github.actor == 'you'`) and scanned for it.
Tracing the attack showed that is not enough: a workflow file is written by whoever opens the
pull request or pushes the branch, so an attacker deletes the guard in their own copy and
targets the runner. Anything that protects the machine must live where they cannot edit it.
The scan stays as defence in depth (it catches honest mistakes and keeps jobs from being
queued for the runner) and says so in its docs and messages.

The hook policy (`internal/hook`) allows a job only if all of these hold, and refuses whenever
a fact is missing:

1. the repository is the configured one (or, for organization scope, belongs to the
   organization);
2. the actor, and the triggering actor if different, are in `runner.trusted_actors` (default:
   the repository owner; required for organization scope);
3. the event is one only people with write access can cause (`push`, `workflow_dispatch`,
   `schedule`, `merge_group`) or a `pull_request`;
4. a pull request's head repository is the repository itself, so a trusted actor who updates
   someone's fork branch does not run the fork's code.

`apply` installs the script and policy under `~/.bladerunner/runners/<name>/hooks/`, points the
runner at the script through `.env`, repairs tampering, and restarts a running runner so it
picks the hook up. The script calls the `bladerunner` program back, so the program's path is
recorded in it; if the program moves, every job is refused (fail closed) until `apply` is run
again, and `doctor` reports it.

## Assumptions to verify in BR-0

| # | Assumption | If it is wrong |
|---|------------|----------------|
| H1 | The runner reads `ACTIONS_RUNNER_HOOK_JOB_STARTED` from `.env` | The hook never runs and **nothing enforces**. The most important thing to verify. |
| H2 | The hook runs before any step and a non-zero exit fails the job | As H1, or untrusted steps start before the check. |
| H3 | `GITHUB_REPOSITORY`, `GITHUB_ACTOR`, `GITHUB_EVENT_NAME` reach the hook | The hook refuses every job; safe, but unusable. |
| H4 | `GITHUB_EVENT_PATH` is readable for pull requests | The hook refuses every pull request; safe. |
| H5 | `GITHUB_ACTOR` is the person who caused the run and `GITHUB_TRIGGERING_ACTOR` the re-runner | A re-run could be attributed to the wrong person. |
| H6 | `GET /repos/{r}/actions/permissions/fork-pr-contributor-approval` returns `approval_policy` with `all_external_contributors` as the strictest value | `apply` cannot verify; the user confirms with `--fork-approval-confirmed`. |

The BR-0 runbook (`docs/BR-0-runbook.md`) tests H1 to H5 directly: it changes the trusted actor
to someone else and confirms the owner's own job is refused with the hook's message.

## Choices

- **Fail closed everywhere.** Missing policy, unreadable payload, unknown event, missing
  variable, moved program: the job is refused.
- **The hook is the program itself** (`bladerunner hook job-started`), not a shell script
  parsing JSON, so it is one tested code path and needs no `jq`.
- **The workflow guard accepts one exact shape**, textual and strict, and `CanonicalIf` emits
  it, so `generate` (BR-3) and the scan cannot drift apart.
- **Checkout-matches-config** is skipped with a note when there is no git checkout, no origin,
  or another host, rather than guessed.
