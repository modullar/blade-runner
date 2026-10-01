# Decision records

One record per decision the implementation depends on. The spec (`BladeRunner_Implementation_Prompt.pdf`,
v1.0 draft) marks several assumptions `VERIFY` and says to resolve them in milestone BR-0
against current GitHub documentation and a real machine.

**BR-0 has not been done.** The implementation so far was built and tested in a Linux
sandbox with no Mac, no throwaway GitHub repository, and no route to the live GitHub API for
`actions/runner`. So every item below that depends on GitHub's real behavior is an
**assumption encoded in code and in the test fake `githubtest`**, not a confirmed fact. The
fake can only confirm the code agrees with itself.

| ID | Spec assumption | Status | Where it lives |
|----|-----------------|--------|----------------|
| A1 | Implementation in Go | **Decided** with the requester; see [0001](0001-language-and-dependencies.md) | whole repo |
| A2 | Runners API lists runners; token scope | **Open**: endpoints assumed, minimum scope unknown; see [0002](0002-token-scope.md) | `provider/github/client.go` |
| A3 | Registration token via API, unattended `config.sh` | **Open**: flags and env input assumed; see [0003](0003-registration-and-release.md) | `install/apply.go` |
| A4 | A job queued for an offline label waits, up to a limit | **Open**; the forge code claims "up to 24 h", unverified | `doctor` warns on `placement: local` |
| A5 | `gh` and `jq` preinstalled on `ubuntu-latest` | **Open**; not used until BR-3 (the preflight job) | not yet |
| A6 | macOS runner as a per-user LaunchAgent | **Open**: command sequence matches the forge setup's, but unproven on a Mac here | `platform/macos` |
| A7 | Agent port 7878, polling 10 s / 60 s | **Parsed and validated only**; the agent is BR-4 | `config` |
| A8 | Pure-Go SQLite driver | **Not reached** (BR-4). Note: stdlib-only (0001) means BR-4 must decide on a dependency | none |

| H1 to H6 | The runner honors a job-started hook and gives it the job's identity; the fork-approval API | **Open**; see [0005](0005-only-your-code-runs-here.md). H1 is what stops other people's code, so it comes first in BR-0 | `internal/hook`, `install/jobhook.go` |

| C1 to C3 | GitHub exposes a commit's signed bytes, supports one-job just-in-time runners, and names the commit a job would run | **Open**; see [0006](0006-signed-admission-and-isolation.md) | `provider/github`, future supervisor |

Egress (a job that needs the network reaches only allowlisted hostnames, not the host, LAN, metadata or other containers) is [0008](0008-egress.md): built and tested on Linux with real Docker, **not verified on macOS**; it lives in `internal/egress` and the `allowlist` mode of `internal/isolation`.

The current direction, cryptographic admission plus isolated runners, is
[0006](0006-signed-admission-and-isolation.md); it supersedes the login-based hook of 0005 once built.

Decisions that depart from or extend the spec are in [0004](0004-deviations-and-choices.md);
how other people's code is kept off the machine is in [0005](0005-only-your-code-runs-here.md)
and [../security.md](../security.md).
