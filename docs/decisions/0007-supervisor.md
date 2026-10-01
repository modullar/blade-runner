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
  commit, **which is what the job actually runs: see "Open: PRs run the merge commit" below, and
  note that `pull_request` runs are refused unless `supervisor.allow_pull_request_merge: true`.**
  Anything incomplete or contradictory (no head repository, a commit id that is not 40 hex
  digits, a job whose commit differs from its run's, no labels, unknown status) is refused as
  ambiguous (BR-E073), not guessed at.
- **Events.** Only `push`, `workflow_dispatch` and `schedule`, plus `pull_request` when
  `supervisor.allow_pull_request_merge` is true (default false). Others
  (`pull_request_target`, `issue_comment`, `workflow_run`, ...) run code that is not the commit
  that was verified, or at a time a stranger picks: refused (BR-E074).
- **Stateless by design.** The queue, the runners and the containers are read from the provider and
  Docker every time; the only memory is "what did I already write to the audit log" and "how often
  did this job fail to start", both in process. A restart `Reconcile`s: it removes every leftover
  runner container and every runner registration named exactly `br-jit-<runner name>-<12 hex
  digits>` (and nothing else: a prefix match would take `br-jit-mini-2-...`, another supervisor's,
  for `mini`'s),
  then carries on. A job that is still queued is simply judged again; one that is done is not run
  twice because the provider says it is done.
- **The container** is started through `internal/isolation` (read-only root, no capabilities,
  non-root, limits, audited before start), from the image pinned by digest in `supervisor.image`
  (or `--image`), with the just-in-time config on **standard input only**: not in argv, not in the
  environment, not in the audit log, not in the log. The network mode defaults to `bridge`
  because the runner must reach GitHub (see the known limit in 0006).
- **The audit log** is `~/.bladerunner/audit/supervisor-<runner name>.jsonl` (0600 in a 0700
  directory; one file per runner name, and the file is flocked while open, so two supervisors
  cannot fork one chain): append only (`O_APPEND`, fsynced), one JSON object per line, each naming
  the SHA-256 of the line before it, so an edited or deleted line is detectable
  (`supervisor.VerifyAuditLog`). It records every refusal (with its diag code), withholding, cancel
  request, launch (with who vouched: signer and fingerprint), outcome (exit code, timeout, OOM,
  which jobs the runner took) and alarm. **A launch is recorded before the runner is registered,
  and nothing starts if the record cannot be written** (BR-E079), and the supervisor loop stops on
  an audit failure instead of polling on. Failures to record an alarm, a launch failure or a
  withheld launch are returned together with the failure they describe, never dropped.
  Repeated polls of the same refusal are written once: the "already recorded" sets are bounded
  by evicting the least recently used entry (never by emptying the set, which refired every
  refusal on the next poll), and the bound grows to twice the number of jobs currently waiting. This is evidence against accident and
  casual tampering; someone who can write the file can rewrite the chain, so it is not a defence
  against root.
  - *Write failures.* A line that reached the disk is part of the chain even if the flush after it
    fails; after a failed flush, or a write that left part of a line, the writer refuses every
    later write (fail closed) until the log is reopened.
  - *Torn last line.* Reopening a log that ends in a half-written line does not hide it: the
    fragment stays in the file and a `recovered` entry names it by hash, so the chain verifies
    again and the damage stays on record. `VerifyAuditLog` on a not-yet-recovered log reports
    `ErrTornTail` after verifying everything before it. Any other unparseable line is an error.
  - *The head anchor is replaced durably.* The new anchor is written to a temporary file that is
    **fsynced before the rename**, and the directory is **fsynced after it**; without both, a
    power cut can leave a renamed but empty anchor, or no rename at all. An anchor that is
    **empty** is therefore read as exactly that power cut, not as tampering, but only when the log
    itself verifies: `VerifyAuditLog` reports `ErrEmptyAnchor` and reopening the log records a
    `recovered` entry saying the anchor was empty and writes a new one. (An empty anchor is also
    what someone who wanted to disable it would leave, so it is on the record, and an anchor that
    was *deleted* stays the limit stated below.)
  - *Two torn lines.* A crash while the `recovered` entry itself was being written leaves two
    unparseable lines in a row. The next open records a second `recovered` entry that names both
    by hash (`fragments_sha256`, in file order); a third unparseable line in a row is damage.
  - *The last entry has no successor,* so the chain alone cannot see it edited or the tail cut
    off. A head anchor `<log>.head` (number and hash of the newest entry, rewritten after every
    entry) covers that while it survives: verification and opening refuse a log shorter than its
    anchor or whose anchored entry differs. **An empty log verifies clean, and so does one whose
    anchor was removed or rewritten with it**: both are files, so nothing remembers what was
    there. Holding the head hash off the machine is the only real answer and is not built
    (`TestKnownLimit_AnEmptyOrWhollyReplacedLogVerifiesClean`).

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
   withholds (BR-E075/E076). A commit that **provably does not exist** is different: that is a
   refusal (BR-E072), so `--cancel-unadmitted` can clear it. "Provably" is narrow, because GitHub
   answers 404 for a repository the token cannot see and for a commit or fork that was only just
   pushed or created: only HTTP 422 "No commit found for SHA", or a 404 **after a second
   `GET /repos/{repo}` shows the repository itself reads fine with the same token**, is a
   refusal. A 404 with an unreadable repository is "not now": withheld, recorded as such, never
   a refusal and never grounds to cancel anyone's run. (What remains: a commit pushed a moment ago, in a repository that reads fine, whose object GitHub has not made visible yet, answers 404 and is refused until the next poll sees it; that costs one audit record, and a cancel only if `--cancel-unadmitted` is on.) The two are told apart by diag code
   (BR-E067 versus anything else), never by the wording of an error. Runs and jobs are read
   with full pagination, and a list that ends short of its `total_count` (GitHub caps these lists
   at 1000 results, and a page can be short) is an error, never a shorter answer: reading stops
   after one empty page while the total promises more, and then it is an error. The reverse
   (a `total_count` that lags behind the list, say a run created after the count) is not an
   error and never cuts the list at the total: a full page is followed by another until a short or
   empty page, so the extra runs are seen. The run
   statuses are read in **reverse lifecycle order** (requested, pending, waiting, queued,
   in_progress) and in **two passes**, taking the union: a run only moves forward, so one that
   advances between two listings lands in a status not yet read, and one created after its status
   was read is caught by the second pass. A run's jobs are read **once per status it is listed
   in** (not once per run id for the whole scan: a run listed while still empty in `waiting` has
   its jobs by the time it is `queued`, and the empty first reading must not stand for it), and
   once more in the second pass if the first reading found no job; the later reading of a job
   wins. (The listings are still not an atomic snapshot: a run
   that appears and is taken between the last check and the runner connecting is R1 below.)
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
   recorded as withheld). Every waiting job it could take is **judged again each poll**, so a key
   revoked or a commit that is definitively gone (a refusal) while the runner waits stops it too;
   a commit is fetched once per poll however many jobs share it. A job that **cannot be judged**
   (GitHub answers 404 or 5xx) does not by itself kill an admitted runner: it is stopped only after
   three polls in a row **and** only when `GET /repos/{repo}` succeeds in that poll, which makes
   the failure a statement about the commit and not about the connection or the token. While the
   repository cannot be read either, the runner keeps waiting (a hiccup must not cost an admitted
   job its runner; the queue-unreadable rule below still bounds a blind watcher). A single-use runner takes exactly one job, so once it has one the
   exposure ends and the watch stops: a running admitted job is never killed because something
   else arrived. If the queue cannot be read three times in a row while the runner still has no
   job, it is stopped: its exposure is unknown.
5. **Verify afterwards.** When the container ends, the provider is asked which job(s) carried this
   runner's name (running and the newest 30 finished runs). A job that was not admitted is an
   **alarm** (BR-E077, audit record, non-zero exit), and the same check during the run stops the
   container at once. If this look itself fails (GitHub unreadable after the runner ended) the
   supervisor cannot say what the runner took, so that too is an **alarm** entry and a non-zero
   exit (BR-E076), not a note. This is detection, not prevention.
6. **A job that keeps failing** to start or to be taken is attempted three times, then left alone
   until the supervisor restarts, so a broken image cannot spin the loop. A launch that was only
   withheld (re-check failed, queue unreadable, runner stopped for someone else's job) is
   **refunded, but only three times in a row per job**: the fourth gives the job up until a
   restart, because a job whose runner is called off over and over is not getting one either, and
   the loop would otherwise register and tear down runners for it for ever. (The count restarts
   when a launch is not called off.) The loop also **backs off**: after a launch that ran its job
   it starts the next cycle at once, but after one that was withheld or whose runner was stopped
   it sleeps the poll interval like any other idle cycle. A job that was given up on stays given
   up across a scan that happens to miss it (the listings are not an atomic snapshot): it is
   forgotten only after being absent for five scans in a row, or on restart. The audit trail is
   one `gave_up` record.

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

## Open: PRs run the merge commit

**Status: a known hole, not fixed. It needs a decision from the repository owner.**

For a `pull_request` run, GitHub's `head_sha` on the run is the **head of the pull request**, but
the job executes `GITHUB_SHA`, which is the **synthetic merge commit** of that head into the base
branch, and the workflow file is taken from that merge as well. The supervisor verifies only the
head (the run's `head_sha` in the head repository, cross-checked against `pull_requests[]`). The
merge commit is created by GitHub, signed by nobody this machine trusts, and is never fetched or
verified, so anything the base branch holds that was never checked here (an unsigned commit on the
base branch, a base that moved since the pull request was opened) runs inside the container next
to a properly signed head. "The code that runs was signed by a key I trust" is therefore **false
for pull requests**. (The isolation of 0006 still bounds what that code can do; admission is what
does not hold.)

What is in the code now, and why:

- **Refuse by default.** `pull_request` runs are refused (BR-E074, event not allowed) unless the
  config says `supervisor.allow_pull_request_merge: true`. There is no command-line flag: the
  opt-in belongs in the file the owner reviews. I judged this safe to add on my own because it
  only narrows what runs and fails closed; the opposite default would leave the hole open for
  everyone who has not read this section. The cost is real: with the key off, every pull request
  run is a refused job, and a refused job that this runner could take **blocks the runner** until
  it is cancelled (`--cancel-unadmitted`) or finishes elsewhere, so an owner who uses pull
  requests with this runner must either opt in knowingly or label those workflows so this runner
  cannot take them. Opting in restores exactly the earlier behaviour (judge the head, in the head
  repository).
- **A known-limit test** documents the opted-in behaviour:
  `TestKnownLimit_AnOptedInPullRequestVerifiesTheHeadAndNeverTheMergeCommit` asserts that the
  only commit fetched and verified is the head.

Options (none implemented; pick one, then replace this section with the outcome):

| Option | What it closes | Trade-offs |
|--------|----------------|------------|
| **A. Verify the base tip too.** Admit a pull request run only if the head *and* the current tip of the base branch are signed by trusted keys. | Most of it: the merge is of two verified commits. | Needs the base branch and its tip from the run/PR (`pull_requests[].base`, not read today) and a fetch of the base commit. The merge is made later than the check, and the base can move between the check and the job, so a window remains (the base must be re-read at launch and while waiting, like the rest of the watch). A base branch with an unsigned commit on it blocks every pull request until a trusted key signs on top of it. |
| **B. Admit a PR only if its head descends from a verified base tip.** Verify that the head contains (as an ancestor) a base tip that is itself signed, and that the base has not moved past it. | The merge adds nothing the head did not already contain, so the merge tree equals the head tree. | Needs ancestry queries (`compare` API), which the adapter does not have. Fails for any pull request opened before the base moved on, so contributors must rebase constantly. If the base moves after the check the merge differs again; the window is the same as in A. |
| **C. Run the PR head instead of the merge.** Have the runner check out the head commit (set `GITHUB_SHA`/`ref` for the job to the verified head, or fetch and run it explicitly). | All of it: what runs is exactly what was verified. | GitHub decides what the runner executes: a just-in-time runner cannot be told to run a different commit, and the workflow file already comes from the merge. It needs support in the runner image's job hook or a different trigger (for example, only `push` to the contributor's branch, which gives up the pull request workflow). Not buildable in the supervisor alone. |
| **D. Keep refusing** (the current default). | All of it, by not running pull requests. | PR checks need another runner (a hosted one, or a runner that runs only code already on a trusted branch), and refusal still blocks the queue unless cancelled. |

My recommendation is D now, then C if the runner image's job hook can pin the commit (it is also
what R5 above needs), and A only if pull request checks on this machine are a requirement, with
the residual window stated. This needs the owner's call because every option changes who can get
CI feedback on this machine.

## Assumptions about GitHub (UNVERIFIED; every one lives in `internal/provider/github`)

The supervisor sees only `provider.Run`, `provider.Job` and the `Provider` methods; a BR-0
correction changes the adapter and, at most, the field comments in `provider.go`.

| # | Assumption | If wrong |
|---|------------|----------|
| C1 | `GET /repos/{r}/git/commits/{sha}` returns the signed payload and signature (0006) | Admission cannot fetch commits: every job is refused/withheld (fails closed) |
| C2 | `generate-jitconfig` makes a single-use runner | Needs another way to bind a runner to a job; the supervisor's launch step fails (BR-E078) |
| C3 | A run's `head_sha` is the commit it was triggered for; for a pull request the PR **head**, not the merge commit (but see "Open: PRs run the merge commit": the job runs the merge commit); `head_repository.full_name` is where it lives; `pull_requests[]` (possibly empty for forks) agrees | If `head_sha` is the merge commit it is unsigned, so every PR job is refused (BR-E072); if it disagrees with `pull_requests[]` the job is refused as ambiguous (BR-E073). Nothing runs on a wrong guess |
| C4 | `POST /repos/{r}/actions/runs/{id}/cancel` cancels a queued run (answers 202) | `--cancel-unadmitted` records a failed cancel (BR-E078) and the queue stays blocked: safe, not live |
| C5 | A job lists `runner_name` once a runner has taken it, and `status` is one of `queued`, `in_progress`, `completed`, `waiting`, `pending`, `requested` | See R2; an unknown status is treated as "might still be taken" |
| C6 | The run list can be filtered by `status` and both lists paginate with `per_page`/`page` and `total_count` | A list that ends short of `total_count` (GitHub stops these lists at 1000 results; a page can be short) is an error (BR-E076/E022), never a shorter answer |
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
- **One supervisor per runner name** (a directory lock, and a lock on its own audit log); two would reconcile each other away.
- **API cost.** Each cycle lists runs for five statuses twice (two passes) and the jobs of each
  run once (the second pass reads jobs only for runs it had not seen); the watcher does the same
  every 15 s while a runner waits, plus one commit fetch per distinct commit of a waiting job.
  That is fine for a small queue and would need `?created` filtering or conditional requests for
  a busy repository. Jobs of a run that was already read are not read again within one scan, so a
  job added to an already-read run between the two passes is only seen at the next scan.
- **The post-run look at finished runs is bounded** to the newest 30, and costs one page of 30
  (`ListRecentRuns`), not every page; a job that completed and fell off that window before the
  check would be missed (the watcher normally sees it first).
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
