# 0007: The supervisor (increment 3 of 0006)

**Status:** built and tested against the in-process fake GitHub, real `git`/`ssh-keygen`
commits and a **real Docker daemon**. **Not run against the real GitHub.** One hazard, the
shared-queue race below, is **mitigated, not solved**, and BR-0 must verify what it leans on.
Pull requests are admitted only when every commit that can reach the job is signed by a trusted
key ("Pull requests: every commit verified" below); that design leans on GitHub behaviour (C10 to
C12b) that is **assumed, not verified**.

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
  in this repository. For a pull request: the base tip, **every commit of the pull request**, and
  that the merge commit the job runs is made of exactly those (see "Pull requests: every commit
  verified" below). Anything incomplete or contradictory (no head repository, a commit id that is
  not 40 hex digits, a job whose commit differs from its run's, no labels, unknown status) is
  refused as ambiguous (BR-E073), not guessed at.
- **Events.** Only `push`, `workflow_dispatch`, `schedule` and `pull_request` (with the check
  above). Others (`pull_request_target`, `issue_comment`, `workflow_run`, ...) run code that is
  not the commit that was verified, or at a time a stranger picks: refused (BR-E074).
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

## Pull requests: every commit verified

**Status: decided by the repository owner ("each contributor needs to be trusted and signed in"),
built and tested against the fake GitHub, real git and a real Docker daemon. The GitHub behaviour
it relies on (C10 to C12b below) is UNVERIFIED and needs BR-0.** This replaces the earlier
refuse-by-default switch `supervisor.allow_pull_request_merge`, which is **removed** (a config that
still sets it fails with an error that says so and why).

### The problem it closes

For a `pull_request` run, GitHub's `head_sha` on the run is the head of the pull request, but the
job executes `GITHUB_SHA`, the **synthetic merge commit** of that head into the base branch, and
the workflow file is taken from that merge too. The merge commit is created by GitHub and signed
by nobody this machine trusts, so verifying only the head left base-branch content nobody checked
here (an unsigned commit on the base branch, or a base that moved) running next to a signed head.

### The rule

A `pull_request` run is admitted only if **all** of these hold. Each failure is a refusal
(BR-E072, with a detail naming the exact commit and why), except data that cannot be read or does
not exist yet, which is a transient withhold (BR-E076, retried at the next poll, never recorded as
a refusal and never grounds to cancel a run):

```
run ── names exactly one pull request ─────────────────────────────── else refuse
  1. GET pulls/N        open · number is N · base repo is this one · base branch = the base ref
                        the run's own pull_requests[] entry records (refuse when it differs) ·
                        head sha = the run's head sha · head repository = the run's · mergeable
                        true · merge_commit_sha present · base sha = the base sha the run's entry
                        records (it differs: the base moved since the run was created: withhold)
                        (mergeable null or the PR unreadable: withhold; false or closed: refuse)
  2. GET commit M       the merge commit, from the BASE repository:
                        exactly two parents · parents[0] = base.sha · parents[1] = head sha
  3. admit base.sha     the base tip: signed by a trusted, unrevoked, unexpired key
  4. GET pulls/N/commits  every commit of the pull request (>= 250 listed, or fewer listed than the
                        pull request has: refuse as too many commits to verify) · the head is among
                        them · each is fetched from the base repository, then the head repository
                        if the base cannot serve it, and admitted; a commit neither can serve:
                        withhold
  5. any unsigned, RSA/ECDSA/GPG-signed, unknown-signer, revoked or expired signer on 3 or 4:
     refuse the whole job, naming the commit (and the author as GitHub reports it)
```

Step 2 is what binds "what will run" to "what is verified": the job's `GITHUB_SHA` is the merge
commit, whose tree is the merge of two verified trees. Refusals outrank transients (a commit that
failed is the answer whatever else could not be fetched). The commits of one pull request are
verified by a bounded pool of 4 workers; after a refusal the commits beyond it that are not yet
started are skipped (an API call saved at every poll), so the detail does not name every bad
commit. **It always names the lowest-index bad commit, whatever the timing of the workers:** a
commit before the lowest refusal found so far is never skipped, so the refusal text, which goes
into the audit log through `recordOnce`, cannot flap between two bad commits from poll to poll.
The commit list is de-duplicated (a sha listed twice is verified, and audited, once). The result
slot of a commit is "unset" until written, and unset is not admitted: only a result that says
admitted lets a pull request through. A pull request is read once per assessment however many
jobs its run has, and that verdict is remembered per (number, head repository, head sha, the base
ref and base sha the run records), so two runs of one pull request number (a stale run and the
current one, or one naming another repository) are each judged on their own.

**The run's own `pull_requests[]` entry is compared with the pull request that was fetched.**
Two open pull requests can have the same head commit into different bases, and a run lists one of
them. If the base **branch** the run's entry records differs from the fetched pull request's, the
run is not for this pull request as it stands: refused (BR-E072, the detail names both branches).
If only the base **sha** differs, the base moved since the run was created: withheld (BR-E076) and
judged again at the next poll, never refused. The run's `GITHUB_SHA` may then be the merge onto
the OLD tip, which nothing here verified (see (f)); withholding is the safe answer, and a run
that records a base sha that never catches up stays withheld until it is superseded by a newer
run (a push to the pull request). A run entry that carries no base ref or sha (C10b, below) is not
compared on that field. A pull request endpoint that answers with another number is refused.

**Re-judged at every poll, and while a runner waits.** Nothing about a pull request is cached
across polls except the fetch of commit objects (below). The judgement carries the merge commit
it was made for, and a runner is only kept while that stays the same: the re-read after the
runner is registered withholds the launch if the merge commit changed since the job was admitted
(the base moved, or the head was pushed to), and the watcher stops a runner that has not yet been
handed a job when a waiting pull request job's merge commit differs from the one it was admitted
at, or when anything on the new one no longer passes. The next poll judges the new merge commit
from scratch and runs it if it passes. A runner that already has its job is not killed
(unchanged rule).

**When a waiting job cannot be judged for 3 polls in a row**, the watcher stops the runner only if
the repositories on the failing side read fine: the base repository when the pull request, the
merge commit, the base tip or the commit list could not be read, the base and the fork when a
commit could not be fetched. Before, only the head repository was asked, so a fork that read fine
stopped a runner whose real problem was an unreadable base, and a gone fork kept alive a runner
whose base side was failing with the base readable.

**The audit record.** The `launching` entry of a pull request lists the merge commit (`merge_sha`)
and every verified commit (`verified_commits`: role `base-tip` or `pr-commit`, the repository it
was fetched from, sha, signer name and fingerprint), at most 50 and `verified_total` for the
count. When there are more than 50, the **base tip and the head commit are always listed** (with
their signers: they are the two commits the merge commit is made of), and the rest are the oldest
others in order, up to the bound.

### What it costs, in API calls

For one pull request with N commits (N <= 100) the first judgement makes **3 + P + N** requests:
the pull request, the merge commit, the base tip, P pages of the commit list (P = 1 for up to 100
commits, at most 3), and one fetch per commit; plus one repository read per commit GitHub
answers 404 for. A commit never changes under its id, so the supervisor's admitter
(`admit.CommitCache`, 4096 commits and 16 MiB, whichever is reached first, oldest evicted first; a
commit bigger than the whole byte budget is not kept; the key names the repository, so a commit
cached for one repository is never served for another) remembers what it fetched, and **only the fetch**: the
signature is checked against the trust store as it is now at every poll, so a revocation still
takes effect at the next one. Every later poll (and every 15 s watcher poll while a runner waits)
therefore costs **2 + P** requests (the pull request, the commit list pages, the merge commit
which is read fresh) plus the commits it has not seen. Without that cache a 20-commit pull
request would cost over 5000 requests an hour, GitHub's limit for a token, at the 15 s poll. Only
bytes that really hash to the commit id are cached, and an unsigned commit is not (it is cheap to
refuse again).

### What this does NOT give you (read before relying on it)

- **(a) Commits made by GitHub's web UI are signed by GitHub's key, not the owner's.** The merge,
  squash and rebase buttons, edits made in the web editor, and "Update branch" all create commits
  that GitHub signs with its own key. They are therefore **refused** unless GitHub's signing key
  is added to the trust store, and doing that would trust **everything GitHub signs on anyone's
  behalf**: every account with write access, or with a pull request GitHub merges for them, could
  get code onto this machine through that one key. That is the trade-off, stated plainly; this
  record does not name or ship GitHub's key, and it is the owner's decision whether to add it. The
  practical consequence is that a pull request containing a web-UI commit does not run here, and
  a base branch whose tip came from the squash button blocks every pull request until a trusted
  key signs a commit on top of it.
- **(b) Base history before the tip is not individually verified.** Only the tip of the base
  branch is checked. It commits to its whole parent chain and tree by hash (SHA-1), which is the
  git trust model: a signed tip vouches for what is under it, assuming the hash is not broken. An
  unsigned commit deep in the base history does not stop a pull request.
- **(c) The merge algorithm is run by GitHub and is not verified here.** The supervisor checks
  that the merge commit has the right two parents, not that its tree is the correct merge of
  them (that would need a local `git merge-tree`, which is not built). A GitHub that produced a
  tree with content from neither parent would not be caught.
- **(d) Cost:** each pull request costs about 3 + N API calls to admit, see above.
- **(e) First-time contributors must be added with `bladerunner trust add` BEFORE their pull
  request can run.** Until then it is refused (BR-E072, naming their key's fingerprint), and a
  refused job that this runner could take blocks the runner (BR-E075) until it is cancelled
  (`--cancel-unadmitted`) or finishes elsewhere. This is the owner's requirement, and it is also
  what makes the supervisor unsuitable for a repository open to unknown contributors.
- **(f) The run's own `GITHUB_SHA` is not visible to the supervisor.** What is verified is the
  pull request's **current** merge commit. A run created before the base moved executes the
  merge commit of its own time, which the API fields read here do not name. If GitHub keeps such
  a run on its old merge commit, that merge was made against an older base tip that was never
  verified here. The re-judge on every poll narrows this (a moved base withholds the runner and
  the new merge is verified) but cannot close it. Whether a re-created merge is what a queued
  run executes is part of what BR-0 must establish (see C10). The comparison with the run's own
  recorded base (above) turns the most visible case, a base that moved since the run was created,
  into a withhold, but only to the extent GitHub keeps `pull_requests[].base.sha` as of the run's
  creation (C10b).
- **(g) A run from a fork may name no pull request.** The run's `pull_requests[]` is empty for
  runs from forks, as far as is recalled (C3, unverified). A run that names zero pull requests is
  refused, because which pull request, and so which merge commit, it is for cannot be told. If
  that is how GitHub behaves, **fork pull requests will all be refused until it is fixed**
  (for example by looking the pull request up by head commit, which is not built). The probe
  (C3, C10, C12b) reports this.
- **(h) Exactly 250 commits is refused too.** GitHub lists at most 250 commits of a pull request,
  so a list of 250 may have been cut; it is refused like a longer one.
- The merge commit's parents are GitHub's word: that object is unsigned, so unlike the others they
  are not covered by a signature.

### Deviations from the brief the owner gave

- `ListPullRequestCommits` returns the commit ids with GitHub's display author (unverified, for
  messages only) rather than bare ids, and the pull request carries GitHub's own commit count
  (`PullRequestInfo.Commits`), which is what makes a short list detectable.
- Besides "at the cap", a list shorter than the pull request's own count is refused as too many
  commits to verify.
- The admitter did not cache before; `admit.CommitCache` is new (the per-assessment memory in the
  judge was not enough for the cost above) and is wired in `bladerunner supervise` only.
- A run naming zero pull requests is refused as BR-E072 (not BR-E073): it is a failure of the
  rule above, not an inconsistency in the description.
- A merge commit that changes is handled by withholding the launch or stopping the waiting runner
  and judging again at the next poll, rather than by re-verifying inside the same call.
- The base tip is `base.sha` of the pull request, not a fresh read of the branch: the merge was
  computed against it, and step 2 checks that the merge commit says so.

## Unresolved from the earlier version of this record

A per-job "run the head instead of the merge" binding (the old option C) still needs the runner
image's job hook and is the only layer that would prevent rather than narrow R1 and R5; it is
not built.

## Assumptions about GitHub (UNVERIFIED; every one lives in `internal/provider/github`)

The supervisor sees only `provider.Run`, `provider.Job` and the `Provider` methods; a BR-0
correction changes the adapter and, at most, the field comments in `provider.go`.

| # | Assumption | If wrong |
|---|------------|----------|
| C1 | `GET /repos/{r}/git/commits/{sha}` returns the signed payload and signature (0006) | Admission cannot fetch commits: every job is refused/withheld (fails closed) |
| C2 | `generate-jitconfig` makes a single-use runner | Needs another way to bind a runner to a job; the supervisor's launch step fails (BR-E078) |
| C3 | A run's `head_sha` is the commit it was triggered for; for a pull request the PR **head**, not the merge commit (the job runs the merge commit: see "Pull requests: every commit verified"); `head_repository.full_name` is where it lives; `pull_requests[]` agrees, and names exactly one pull request | If `head_sha` is the merge commit it is unsigned, so every PR job is refused (BR-E072); if it disagrees with `pull_requests[]` the job is refused as ambiguous (BR-E073); if `pull_requests[]` is empty (as is recalled for forks) the run is refused (BR-E072). Nothing runs on a wrong guess |
| C4 | `POST /repos/{r}/actions/runs/{id}/cancel` cancels a queued run (answers 202) | `--cancel-unadmitted` records a failed cancel (BR-E078) and the queue stays blocked: safe, not live |
| C5 | A job lists `runner_name` once a runner has taken it, and `status` is one of `queued`, `in_progress`, `completed`, `waiting`, `pending`, `requested` | See R2; an unknown status is treated as "might still be taken" |
| C6 | The run list can be filtered by `status` and both lists paginate with `per_page`/`page` and `total_count` | A list that ends short of `total_count` (GitHub stops these lists at 1000 results; a page can be short) is an error (BR-E076/E022), never a shorter answer |
| C7 | The `labels` passed to generate-jitconfig are accepted as the runner's labels; GitHub's own implicit labels are added or not | See R4; the call fails (BR-E078) if refused |
| C8 | A JIT runner exits after one job and removes its own registration | The supervisor lists runners and removes the registration if it is still there, so "already gone" is the normal case and not an error |
| C9 | The runner image's entrypoint reads the JIT config from standard input and starts the runner (the real runner takes it as `--jitconfig` or an environment variable, so the image needs a small wrapper) | The image is not built here; the test image is a FROM-scratch fixture that only checks the config arrived on stdin |
| C10 | **UNVERIFIED (needs BR-0).** `GET /repos/{r}/pulls/{n}` returns `state`, `base.{ref,sha,repo.full_name}`, `head.{sha,repo.full_name}` (null for a deleted fork), `commits`, `mergeable` (null until GitHub computes it) and `merge_commit_sha`; for an open, mergeable pull request `merge_commit_sha` is the commit a `pull_request` job runs (`GITHUB_SHA`) | A missing field refuses the job (BR-E072); null `mergeable` withholds it. If `merge_commit_sha` is NOT what the job runs, the admitted merge is not the executed one (see (f)) and this design does not hold |
| C11 | **UNVERIFIED (needs BR-0).** `GET /repos/{r}/pulls/{n}/commits` lists every commit of the pull request, paginated at 100, up to a cap of 250 | A list that stops short of the pull request's own count, or reaches 250, is refused as too many commits |
| C12 | **UNVERIFIED (needs BR-0).** The merge commit (`GET /repos/{r}/git/commits/{merge sha}`) lists exactly two `parents`, `parents[0]` the base tip (`base.sha`) and `parents[1]` the pull request head | Any other shape is refused; if GitHub orders them the other way every pull request is refused (fails closed) |
| C10b | **UNVERIFIED (needs BR-0).** A run's `pull_requests[]` entries carry `base.ref` and `base.sha` (the branch and its tip when the run was created) | An entry without them is not compared on that field (no refusal, no withhold): the check is weaker, not wrong. A `base.sha` that GitHub keeps current would make the moved-base withhold never fire; one that is never updated withholds a run for ever (until a newer run supersedes it) |
| C12b | **UNVERIFIED (needs BR-0).** The commits of a pull request from a fork can be read with `GET /repos/{base}/git/commits/{sha}`, with their `verification.signature` and `verification.payload` | The supervisor then asks the fork's repository; a commit neither serves withholds the job |
| C13 | **UNVERIFIED, FROM MEMORY (needs BR-0).** When GitHub's signature verification service cannot answer, `verification.signature` is null or empty and `verification.reason` is `gpgverify_unavailable` or `gpgverify_error`; every other reason with no signature (`unsigned`, `unknown_key`, ...) means there is nothing to verify | **The two reason names were written from memory and have not been observed; the list is in `internal/provider/github/client.go` (`verificationUnavailable`) and is probably incomplete.** Such an answer is "not now": the commit is withheld (BR-E076), never refused as unsigned. A reason name GitHub uses for an outage that is missing from the list makes the commit read as unsigned: refused, which fails closed (a refusal, not a run). A signature that came back is verified by this machine whatever the reason says |

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
  A pull request adds 3 + P + N requests the first time and 2 + P at every later poll (see "Pull
  requests: every commit verified"). That is fine for a small queue and would need `?created`
  filtering or conditional requests for a busy repository. Jobs of a run that was already read are not read again within one scan, so a
  job added to an already-read run between the two passes is only seen at the next scan.
- **The post-run look at finished runs is bounded** to the newest 30, and costs one page of 30
  (`ListRecentRuns`), not every page; a job that completed and fell off that window before the
  check would be missed (the watcher normally sees it first).
- **A refusal is judged again every poll** (revocation takes effect at once; across polls only the
  fetch of a commit object is remembered, never a verdict); the audit log records it once per reason.
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

### Pull requests: how they were tested, and the mutation check

Every commit in these tests is real: `internal/testrig.GitWorld` makes them with the real `git
commit -S` and `ssh-keygen`, one freshly generated ed25519 key per contributor, with the machine's
git configuration switched off and `gpg.ssh.program=ssh-keygen` and an explicit private key on
every signature (the sandbox's own key is never used), and makes GitHub's kind of unsigned merge
commit with `git merge --no-ff`. The fake GitHub serves them with the pull request endpoint, the
commit list (capped at 250 like GitHub's) and the parents of each commit.

Tests (`internal/supervisor/pullrequest_test.go`, `internal/cli/supervise_e2e_test.go`, the probe,
client, admit and config packages): the happy path (base tip + 3 commits by 3 contributors:
admitted, launched, the audit lists every commit, signer and fingerprint, and the merge commit); a
fork pull request that only the fork serves; an untrusted contributor on a fork pull request
(refused, nothing registered or started); an unsigned commit in the middle; an unsigned base tip;
a base tip by a stranger; revoked and expired signers (on a commit and on the base tip); a merge
commit with swapped, wrong, one, three or no parents; `mergeable` null (withheld, then admitted
once known), false, and a merge commit id missing; the merge commit, base tip, commit list or
pull request unreadable (withheld, never refused); 300, exactly 250 and a cut-short list of
commits (refused); a commit that cannot be fetched (withheld, also for a 404); a refusal that
outranks an unfetched commit; a run whose head is not the pull request's; closed, foreign-base,
foreign-head and head-less pull requests; zero or two pull requests on a run; a head not in the
list; `pull_request_target` (still refused, no pull request calls); a matrix of jobs reading the
pull request once; the API cost (5 commit fetches and 2 pull request calls for 3 commits); the
50-commit bound on the audit record; the base moving between judgement and launch (nothing starts,
the new merge commit is verified at the next poll), moving to an unsigned tip while a runner waits
(stopped), moving to another signed merge commit while a runner waits (stopped), and moving after
the runner has its job (not killed); revocation applying at once through the commit cache; and,
through the real command line with a real Docker container, a pull request of three commits that
runs and one with an untrusted contributor that is refused (and runs once they are trusted), plus
the removed config key.

Hardening round (independent review of the above), each item with a test written first and each
test shown to fail when the fix is removed by hand: the run's recorded base ref and sha against the
fetched pull request (two open pull requests with one head into different bases; a moved base;
another number), the provider reading `base` out of the run listing; the zero value of the
admission class not admitted (`firstNotAdmitted` over an unwritten slot); the audit bound keeping
the base tip and the head of a 120-commit pull request; the verdict key (two runs of one number
with another head sha and another head repository in one assessment, each key part mutated
separately); a sha listed twice; the commit cache keyed by repository, and bounded by bytes with
FIFO eviction and an oversized entry that flushes nothing; the early stop (at most 8 of 41 commit
fetches after a refusal on the first) and the lowest bad commit named every time (an internal test
holds back the worker of commit 1 until commit 3 has been refused, and a loop of 40 polls);
verification reason `gpgverify_unavailable`/`gpgverify_error` withheld, other reasons unsigned
(C13); the watcher asking the failing side (both directions, with a fork); the probe's C10
wording, C12b requiring signature and payload (a signature without payload fails; commits that
are all unsigned SKIP), and the summary and final line when C10 to C12b were skipped.

Mutation check by hand, 45 mutants, each removed or inverted in turn, each killed by at least one
test: exactly-one pull request
(both directions); base repository; open state; run head versus pull request head; gone head
repository; head repository mismatch; malformed base sha; mergeable null refused instead of
withheld; mergeable false; missing merge commit id; unreadable merge commit refused instead of
withheld; parent count; first parent is the base tip; second parent is the head; base tip never
admitted; unreadable base tip, commit list and pull request each refused instead of withheld; the
cap check and the count check of truncation, separately; an unusable commit id in the list; head
among the listed commits; the fallback to the head repository; an unfetched commit refused
instead of withheld; a refusal no longer outranking an unfetched commit; a definite not-found
treated as a refusal; only the head commit verified; the launch re-read ignoring a moved merge
commit; the watcher ignoring one; a pull request judged as a push; `pull_request_target`
allowed; the audit bound; the merge commit missing from the launch record; the head's verdict;
the cache keeping bytes that are not the commit; the removed config key accepted; the client
reading null `mergeable` as false, reversing the parents, reading one page of commits, and losing
the no-such-commit marker; the probe passing a skip, ignoring the parent order, the head
membership and the fork readability. Three mutants did not compile at first (an unused import,
a duplicate field) and were rewritten as valid ones; all then failed a test. Not mutated: the
`Author` text, the order of the audit fields, and the log wording.

Not verified by any test here: that GitHub behaves as C10 to C12b say, which is what the probe
(C10, C11, C12, C12b) checks in BR-0 and SKIPs, never passes, when no pull request exists. When
any of them was skipped the probe's summary says "N checks SKIPPED, BR-0 is NOT complete" and its
last line starts with "BR-0 INCOMPLETE"; the exit status stays 0 (nothing failed), so read the
last line, not the status. C10 now passes on what is observed (the fields are present and well
formed) and says in its detail that it did NOT observe that a job's `GITHUB_SHA` is the
`merge_commit_sha`: compare it with a pull_request job's log. C12b additionally needs the fork
commits' signature and payload to arrive; fork commits that are all unsigned SKIP it. C10b and C13
have no probe check: C10b is read from the run listing the supervisor already uses, and C13 can
only be observed when GitHub's verification service is down.
