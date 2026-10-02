# Decision records

One record per decision the implementation depends on. The spec (`BladeRunner_Implementation_Prompt.pdf`,
v1.0 draft) marks several assumptions `VERIFY` and says to resolve them in milestone BR-0
against current GitHub documentation and a real machine.

**BR-0 has been done only in part (2026-10-02): see [BR-0-results.md](BR-0-results.md) for what was observed and what was not.** Before that it had not been done at all. The implementation so far was built and tested in a Linux
sandbox with no Mac, no throwaway GitHub repository, and no route to the live GitHub API for
`actions/runner`. So every item below that depends on GitHub's real behavior is an
**assumption encoded in code and in the test fake `githubtest`**, not a confirmed fact. The
fake can only confirm the code agrees with itself.

| ID | Spec assumption | Status | Where it lives |
|----|-----------------|--------|----------------|
| A1 | Implementation in Go | **Decided** with the requester; see [0001](0001-language-and-dependencies.md) | whole repo |
| A2 | Runners API lists runners; token scope | **Partly confirmed** (listing runners works with the owner's token; the permissions used were not reported, so the minimum scope is unknown); see [0002](0002-token-scope.md) | `provider/github/client.go` |
| A3 | Registration token via API, unattended `config.sh` | **Open**: flags and env input assumed; see [0003](0003-registration-and-release.md) | `install/apply.go` |
| A4 | A job queued for an offline label waits, up to a limit | **Open**; the forge code claims "up to 24 h", unverified | `doctor` warns on `placement: local` |
| A5 | `gh` and `jq` preinstalled on `ubuntu-latest` | **Open**; not used until BR-3 (the preflight job) | not yet |
| A6 | macOS runner as a per-user LaunchAgent | **Open**: command sequence matches the forge setup's, but unproven on a Mac here | `platform/macos` |
| A7 | Agent port 7878, polling 10 s / 60 s | **Parsed and validated only**; the agent is BR-4 | `config` |
| A8 | Pure-Go SQLite driver | **Not reached** (BR-4). Note: stdlib-only (0001) means BR-4 must decide on a dependency | none |

| H1 to H6 | The runner honors a job-started hook and gives it the job's identity; the fork-approval API | **Open**; see [0005](0005-only-your-code-runs-here.md). H1 is what stops other people's code, so it comes first in BR-0 | `internal/hook`, `install/jobhook.go` |

| C1 to C3 | GitHub exposes a commit's signed bytes, supports one-job just-in-time runners, and names the commit a job would run | **Confirmed in part** by the owner's run (ed25519 SSH commits on one repository; JIT runner registration; run and job fields; no forks): see [BR-0-results.md](BR-0-results.md). See [0006](0006-signed-admission-and-isolation.md) | `provider/github`, future supervisor |

| C4 to C9 | The supervisor's further assumptions: run cancellation, `runner_name` on jobs, job statuses, JIT labels, pagination, the runner image reading its config from stdin | **Open**; see [0007](0007-supervisor.md), which also states the shared-queue race it narrows but does not close | `provider/github`, `internal/supervisor` |

| C10 to C12b | Pull requests are admitted only when the base tip and **every** commit of the pull request are signed by a trusted key and the merge commit has exactly those two parents: the pull request endpoint's `mergeable` and `merge_commit_sha`, the commit list (cap 250), the order of the merge commit's parents, and fork commits readable through the base repository | **Decided** by the owner, built and tested; GitHub's behaviour **confirmed for a same-repository pull request** (C10, C11, C12, and the job running the merge commit: see [BR-0-results.md](BR-0-results.md)); **C12b (forks) still SKIPPED**. Commits made by GitHub's web UI are signed by GitHub's key and are refused unless it is trusted: see [0007](0007-supervisor.md), "Pull requests: every commit verified" | `internal/supervisor/pullrequest.go`, `internal/probe/pullrequest.go` |

Egress (a job that needs the network reaches only allowlisted hostnames, not the host, LAN, metadata or other containers) is [0008](0008-egress.md): built and tested on Linux with real Docker, **not verified on macOS**; it lives in `internal/egress` and the `allowlist` mode of `internal/isolation`.

The current direction, cryptographic admission plus isolated runners, is
[0006](0006-signed-admission-and-isolation.md); it supersedes the login-based hook of 0005 once built.

Decisions that depart from or extend the spec are in [0004](0004-deviations-and-choices.md);
how other people's code is kept off the machine is in [0005](0005-only-your-code-runs-here.md)
and [../security.md](../security.md).
