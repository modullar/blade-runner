# BR-0 results (partial), 2026-10-02

What the owner's real run of `bladerunner probe github --repository modullar/blade-runner` on a
Mac, plus one read of a CI job log, actually showed, and what it did not. Everything not listed
under "Observed" is still an assumption.

The owner ran the probe three times. The first two used a build from `main`, which has none of
the pull-request checks; the third used the branch at `68927a3` (pull request 2) and is the one
that counts for C10 to C12b. The probe printed no secrets; the token's permissions were not
reported.

## Observed

| ID | Result | What was observed |
|----|--------|-------------------|
| A2 | PASS | `GET .../actions/runners` succeeded with the owner's token. Which permissions that token had was **not reported**, so the minimum scope is still unknown. |
| C1 | PASS, narrowly | Of the 7 commits on the default branch, 5 are SSH-signed: for each, the commit id recomputed from the payload and signature GitHub returned, and the signature verified with this repository's own pure-Go verifier. They were all signed by **one** key. One more signed commit is "other: unsupported" (most likely GitHub's own signature on the pull-request merge: inferred, not confirmed) and one is unsigned (inferred to be the first commit). |
| C2 | PASS | A just-in-time runner was registered, returned a config and was removed again, three times (runner ids 21, 22, 23). |
| C3, C3b | PASS | 12 runs: every one had `head_sha`, `event`, a head repository and an actor (11 `pull_request`, 1 `push`, none from a fork). A run's jobs listed labels and `head_sha`. |
| C10 | PASS (one pull request) | Pull request 2 (same repository) reported `state: open`, `mergeable: true`, `merge_commit_sha`, `base.sha`, `head.sha` and a commit count, all well formed. |
| C11 | PASS (one pull request) | 33 commits listed, the head among them, matching the pull request's own count. |
| C12 | PASS (one pull request) | The merge commit had exactly two parents, in this order: `parents[0]` = base tip, `parents[1]` = pull request head. |
| H6 | readable | The API returns the fork-approval policy. It is `first_time_contributors`, not the strictest (`all_external_contributors`). |

**The job runs the merge commit (the assumption in C10 the probe cannot see).** The CI job log
for pull request 2's run shows `actions/checkout` fetching exactly
`+658449230164...:refs/remotes/pull/2/merge`, `git log -1` printing `658449230164...`, and
`HEAD is now at 6584492 Merge 68927a32... into 48870436...`. The probe read the same
`merge_commit_sha` (6584492301...) and the same two parents. So for that run, on a GitHub-hosted
runner, what the job executed was GitHub's merge commit, which is what ADR 0007's design admits.
`GITHUB_SHA` itself was not printed; the checkout derives it from the same reference.

## Not observed (still assumptions)

- C12b: SKIPPED. No pull request from a fork exists. Whether a fork's commits are readable
  through the base repository, and whether `pull_requests[]` is filled for fork runs, is unknown.
  Until then fork pull requests may all be refused.
- Pagination beyond one page (more than 100 commits), the 250 cap, and `mergeable` being null
  before GitHub computes it.
- A pull request whose base moved after the run was created: which merge commit the job then
  runs. (Narrowed by re-judging, not closed; see ADR 0007, "What this does NOT give you".)
- A commit signed with the **owner's own** key: the probe only reads the default branch, and the
  only signer it saw was another environment's key. Nothing here shows what GitHub returns for
  the owner's key, nor for RSA, ECDSA, or a GPG-signed commit made by a person (GitHub's own
  signature, on the merge, was refused as unsupported, as designed).
- C4 to C9 (the supervisor's own assumptions: cancel, `runner_name`, statuses, JIT labels), C13
  (the verification-outage reasons), A3, A4, A6, and H1 to H5 (the job-started hook): nothing in
  this run touched them.
- That one runner started from a JIT config takes exactly one job, and not another queued job
  with matching labels (the race in ADR 0007).
- Egress and isolation on a Mac: `go test ./...` passed on the owner's Mac, but whether the
  real-Docker tests ran or skipped there was not checked.

## What it means

Admission's central GitHub assumptions (C1, C2, C3, C10, C11, C12) hold for same-repository
pull requests and for ed25519 SSH commits on one repository, at one point in time. The design
is not yet shown for forks, for large pull requests, for other signature types, for the owner's
own key, or for the runner actually being bound to one job.
