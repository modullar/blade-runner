# 0007: The supervisor (increment 3 of 0006)

**Status:** built and tested against the in-process fake GitHub, real `git`/`ssh-keygen`
commits and a **real Docker daemon**. **Not run against the real GitHub.** One hazard, the
shared-queue race below, is **mitigated, not solved**, and BR-0 must verify what it leans on.

[0006](0006-signed-admission-and-isolation.md) defined the supervisor as the part that joins
admission and isolation. This records how it was built (`internal/supervisor`,
`bladerunner supervise`), what it assumes about GitHub, and what it cannot promise.

## What it does

```
poll ──▶ read the queue ──▶ for every waiting job this runner could take:
          (runs x jobs)         admit(head commit, head repository)   [internal/admit]
                                    │
              any refused or unjudgeable? ──yes──▶ start NOTHING; audit; (optionally ask GitHub to cancel)
                                    │ no
                                    ▼
      1 audit "launching" ─▶ 2 JIT registration ─▶ 3 read the queue AGAIN ─▶ 4 start ONE container
      (no record, no launch)                          (changed? abort)         (config on stdin; watch)
                                    │
      5 always: remove the container, remove the registration, record which job the provider says it took
```

- **One runner at a time.** A cycle starts at most one container and waits for it; there is no
  concurrency, so two admitted jobs never share a moment in which either runner could take the
  other's neighbour.
- **What is verified.** For a push, schedule or manual run: the run's head commit, which must live
  in this repository. For a pull request: the run's head commit **in the head repository** (the
  fork), cross-checked against the pull request the provider links to the run; never the merge
  commit. Anything incomplete or contradictory (no head repository, a commit id that is not 40 hex
  digits, a job whose commit differs from its run's, no labels, unknown status) is refused as
  ambiguous (BR-E073), not guessed at.
- **Events.** Only `push`, `pull_request`, `workflow_dispatch` and `schedule`. Others
  (`pull_request_target`, `issue_comment`, `workflow_run`, ...) run code that is not the commit
  that was verified, or at a time a stranger picks: refused (BR-E074).
- **Stateless by design.** The queue, the runners and the containers are read from the provider and
  Docker every time; the only memory is "what did I already write to the audit log" and "how often
  did this job fail to start", both in process. A restart `Reconcile`s: it removes every leftover
  runner container and every runner registration named `br-jit-<runner name>-*` (and nothing else),
  then carries on. A job that is still queued is simply judged again; one that is done is not run
  twice because the provider says it is done.
- **The container** is started through `internal/isolation` (read-only root, no capabilities,
  non-root, limits, audited before start), from the image pinned by digest in `supervisor.image`
  (or `--image`), with the just-in-time config on **standard input only**: not in argv, not in the
  environment, not in the audit log, not in the log. The network mode defaults to `bridge`
  because the runner must reach GitHub (see the known limit in 0006).
- **The audit log** is `~/.bladerunner/audit/supervisor.jsonl` (0600 in a 0700 directory): append
  only (`O_APPEND`, fsynced), one JSON object per line, each naming the SHA-256 of the line before
  it, so an edited or deleted line is detectable (`supervisor.VerifyAuditLog`). It records every
  refusal (with its diag code), withholding, cancel request, launch (with who vouched: signer and
  fingerprint), outcome (exit code, timeout, OOM, which jobs the runner took) and alarm. **A launch
  is recorded before the runner is registered, and nothing starts if the record cannot be written**
  (BR-E079). Repeated polls of the same refusal are written once. This is evidence against
  accident and casual tampering; someone who can write the file can rewrite the chain, so it is not
  a defence against root.

## The key risk: a just-in-time runner takes ANY waiting job whose labels match

Started for an admitted job, a runner is just as willing to take an **unadmitted** job that
happens to be waiting with matching labels. That job's workflow comes from whoever opened the pull
request, so a stranger can queue one on demand. Admission of "the" job therefore protects nothing
unless every job the runner could take is admitted.

### What was implemented (layers; each has tests, each was mutation-checked)

1. **Withhold.** A runner is started only when *every* waiting job this runner could take is
   admitted. "Could take" means all the job's labels are among the runner's (case-insensitively);
   a job naming no labels counts as takeable (a wrong "no" is the dangerous direction). "Waiting"
   means *anything not known to be running or finished*, including `waiting`, `pending`,
   `requested` and statuses this code has never heard of, and including jobs of runs that are
   already `in_progress` (a later job of a run whose first job runs on a hosted runner is the
   classic gap). A job that cannot be judged (GitHub unreachable while fetching its commit) also
   withholds (BR-E075/E076). Runs and jobs are read with full pagination; a list too long to read
   completely is an error, never a shorter answer.
2. **Cancel (opt-in).** `--cancel-unadmitted` or `supervisor.cancel_unadmitted: true` makes the
   supervisor ask GitHub to cancel each refused run (`Provider.CancelRun`), so a stranger's
   queued job cannot block the queue for ever. Cancelling is only a request: the supervisor never
   assumes it worked; the next cycle reads the queue again and starts nothing until it is really
   clear (tested with a GitHub that answers 202 and does nothing). It is **off by default**:
   cancelling a run is visible to others and also stops that run's jobs on hosted runners. The
   price of leaving it off is that any waiting unadmitted job blocks this runner until someone
   cancels it, which is an availability problem, not a safety one.
3. **Re-check after registering.** The registration takes time; before the container starts the
   queue is read and judged again, and the launch is abandoned (registration removed) if anything
   not admitted is now waiting.
4. **Watch while waiting.** While the runner has not been handed a job, the queue is polled; any
   new takeable job that was not admitted at launch stops the container (registration removed,
   recorded as withheld). A single-use runner takes exactly one job, so once it has one the
   exposure ends and the watch stops: a running admitted job is never killed because something
   else arrived. If the queue cannot be read three times in a row while the runner still has no
   job, it is stopped: its exposure is unknown.
5. **Verify afterwards.** When the container ends, the provider is asked which job(s) carried this
   runner's name (running and the newest 30 finished runs). A job that was not admitted is an
   **alarm** (BR-E077, audit record, non-zero exit), and the same check during the run stops the
   container at once. This is detection, not prevention.
6. **A job that keeps failing** to start or to be taken is attempted three times, then left alone
   until the supervisor restarts, so a broken image cannot spin the loop.

### What is NOT solved (BR-0 must verify; do not read this ADR as "closed")

The window between the last check and the moment GitHub hands the runner a job cannot be closed
from the host: an attacker who can queue a matching job at exactly that moment can get it to run in
the container. **Nothing in this design prevents that; it narrows the window and detects it.**
Specifically:

- **R1. Which job GitHub hands the runner.** Layers 1 to 4 assume GitHub assigns a job only from
  jobs that exist at the time. If a job queued *after* the runner connected can be taken before
  the watcher's next poll (15 s by default), the code runs; layer 5 only tells you afterwards
  (test: `TestKnownLimit_TheRaceBetweenTheLastCheckAndTheRunnerBeingHandedAJob`). The container
  isolation of 0006 is what limits the damage then, not admission.
- **R2. Whether `runner_name` on a job appears promptly and reliably** (C5 below). The alarm and
  the "has it been given a job" test depend on it. If it is absent or late, the watcher cannot
  tell that the runner has its job and may stop a runner that is already running one (it would
  then fail that job; it never fails open).
- **R3. Cancellation (C4).** Its latency and whether it affects a queued run at all are unknown.
- **R4. Label semantics of a JIT runner (C7).** If the runner is also eligible for jobs with
  labels beyond those the supervisor assumes, the "could take" test under-counts. The supervisor
  registers each runner with the admitted job's own labels and judges against the full label set
  of the host's configuration, which can only over-count; whether GitHub adds implicit labels
  (self-hosted, OS, architecture) to a JIT runner, or rejects them if listed, is unverified.
- **R5. No binding inside the container.** 0006 planned that "the runner's own job-started hook
  refuses any other job it is handed". That belongs to the runner image and is **not built**: the
  supervisor cannot make the runner refuse a job. When it exists it is the only layer that
  prevents (rather than narrows or detects) R1.
- A per-job unique label was rejected: a runner takes any job whose labels it *has*, so an extra
  label on the runner restricts nothing; restricting would need the job to *ask* for the label,
  and the job's workflow is attacker-controlled.

## Assumptions about GitHub (UNVERIFIED; every one lives in `internal/provider/github`)

The supervisor sees only `provider.Run`, `provider.Job` and the `Provider` methods; a BR-0
correction changes the adapter and, at most, the field comments in `provider.go`.

| # | Assumption | If wrong |
|---|------------|----------|
| C1 | `GET /repos/{r}/git/commits/{sha}` returns the signed payload and signature (0006) | Admission cannot fetch commits: every job is refused/withheld (fails closed) |
| C2 | `generate-jitconfig` makes a single-use runner | Needs another way to bind a runner to a job; the supervisor's launch step fails (BR-E078) |
| C3 | A run's `head_sha` is the commit it was triggered for; for a pull request the PR **head**, not the merge commit; `head_repository.full_name` is where it lives; `pull_requests[]` (possibly empty for forks) agrees | If `head_sha` is the merge commit it is unsigned, so every PR job is refused (BR-E072); if it disagrees with `pull_requests[]` the job is refused as ambiguous (BR-E073). Nothing runs on a wrong guess |
| C4 | `POST /repos/{r}/actions/runs/{id}/cancel` cancels a queued run (answers 202) | `--cancel-unadmitted` records a failed cancel (BR-E078) and the queue stays blocked: safe, not live |
| C5 | A job lists `runner_name` once a runner has taken it, and `status` is one of `queued`, `in_progress`, `completed`, `waiting`, `pending`, `requested` | See R2; an unknown status is treated as "might still be taken" |
| C6 | The run list can be filtered by `status` and both lists paginate with `per_page`/`page` and `total_count` | A list that cannot be read completely is an error (BR-E076) |
| C7 | The `labels` passed to generate-jitconfig are accepted as the runner's labels; GitHub's own implicit labels are added or not | See R4; the call fails (BR-E078) if refused |
| C8 | A JIT runner exits after one job and removes its own registration | The supervisor lists runners and removes the registration if it is still there, so "already gone" is the normal case and not an error |
| C9 | The runner image's entrypoint reads the JIT config from standard input and starts the runner (the real runner takes it as `--jitconfig` or an environment variable, so the image needs a small wrapper) | The image is not built here; the test image is a FROM-scratch fixture that only checks the config arrived on stdin |

## Diagnostic codes

BR-E070 and BR-E071 were already used (doctor), so the supervisor's range is **BR-E072 to
BR-E079**: E072 commit not admitted, E073 ambiguous or incomplete description, E074 event not
allowed, E075 launch withheld, E076 queue unreadable, E077 unexpected job (alarm), E078 launch or
cleanup failed, E079 audit log unwritable. Refusals and withholding exit 0 (they are correct
behaviour); E076 to E079 are failures and exit 1.

## Scope and limits

- **Repository scope only.** `supervise` refuses `runner.scope: org`; an organization needs a
  per-repository queue read the adapter does not have yet.
- **One supervisor per runner name** (a directory lock); two would reconcile each other away.
- **API cost.** Each cycle lists runs for five statuses and the jobs of each run; the watcher does
  the same every 15 s while a runner waits. That is fine for a small queue and would need
  `?created` filtering or conditional requests for a busy repository.
- **The post-run look at finished runs is bounded** to the newest 30; a job that completed and
  fell off that window before the check would be missed (the watcher normally sees it first).
- **A refusal is judged again every poll** (revocation takes effect at once, nothing is cached
  across polls); the audit log records it once per reason.
- **Fork pull requests awaiting approval** (`action_required`) have no jobs yet; they are judged
  when they become queued.
- **Not done:** the legacy hook, workflow guard and `trusted_actors` are untouched (increment 4);
  the host-installed runner is still what `apply` installs.

## How it was tested

Real objects wherever possible: the real GitHub client against the in-process fake GitHub
(extended with run cancellation, pull requests on runs, pagination and fault injection), real
signed commits made by `git commit -S` and `ssh-keygen`, the real trust store and verifier, the
real audit log on disk, and a **real Docker daemon** with a FROM-scratch image (`testdata/
fakerunner`, which exits 0 only if the runner config arrived on stdin) for the happy path, the
no-Docker-call-on-refusal path, stale-container cleanup at start-up, and the CLI end-to-end
tests (`internal/cli/supervise_e2e_test.go`). The one stand-in is `scriptedRuntime`, used where a
test must play the GitHub runner inside the container (taking a job at the fake GitHub needs the
network, which a confined container lacks); it still applies `isolation.Spec.Validate`. Tests skip
with a reason if Docker is absent.

Mutation checks: each security check was removed in turn and the suite confirmed to fail (about
40 mutants: admission skipped, each field/consistency refusal, event allowlist, head-repository
choice, provider error treated as admitted or as a refusal, waiting-status and run-status
coverage, label matching, withholding, cancel scope, attempt cap, audit-before-launch, audit
error handling, start-up cleanup scope, image pinning, organization scope, the re-check after
registration, each watcher rule, post-run detection, registration removal, JIT config on stdin
and not in argv, audit chain and permissions, the single-instance lock). In the first round four
did not fail the build: a job/run id consistency check no test reached (a test and a fake-server
fault were added), a swallowed audit error on a refusal (the test passed for the wrong reason
through a later record; it now fails only the refusal record), a mutant that did not compile, and
one equivalent mutant (the watcher stopping its poll after assignment is an optimisation; the
stronger mutant that keeps killing an assigned runner is caught). After those fixes every mutant
fails at least one test.
