# Security model: only your code runs on your machine

> **Direction change, partly built.** The owner's requirement is cryptographic admission (a
> contributor's permission is their public key in a host-held trust store, verified by the host
> against signed commits) plus jobs that run only in isolated containers. See
> [decision 0006](decisions/0006-signed-admission-and-isolation.md). Admission is built and
> tested (`bladerunner trust ...`) but **is not yet consulted when a job runs**, and isolation is
> not built. Until they land, the layers below are what is in force, and they rely on GitHub's
> statement of who the actor is rather than on a signature.

A self-hosted runner executes whatever job GitHub sends it, as you, on your machine. On a
public repository anyone can open a pull request from a fork, and other collaborators can push
branches. Blade Runner's rule is: **code from anyone but the people you name never runs on
your machine, and each person sets up their own runner for their own repository.**

## What stops them

| Layer | What it does | Who can defeat it | Status |
|-------|--------------|-------------------|--------|
| **Job hook** (enforcement) | The runner runs a hook on your machine before every job. It refuses anyone who is not a trusted actor, any repository but yours, any event outsiders can cause, and any pull request from a fork, and it refuses whenever it cannot tell. A refused job fails before any step runs. | Only someone who already controls your machine or your `runner.trusted_actors` setting. It lives on your disk, outside every repository. | Built and tested, but **relies on runner behavior not yet verified** (below) |
| Workflow guard (defence in depth) | `apply` and `doctor` read `.github/workflows` and refuse to go on if a job that can land on your runner is not locked to you by an `if:`, or the workflow uses a trigger outsiders can fire. | **The attacker.** A workflow file is written by whoever opens the pull request or pushes the branch, so they can delete the guard in their copy. | Built and tested |
| Checkout matches config | `apply` refuses when the checkout's `origin` is not the repository the config registers a runner for. A fork cannot inherit your setup and must run its own `init`. | Whoever passes `--allow-repo-mismatch`. | Built and tested |
| Fork-approval setting | On a **public** repository, `apply` requires "Require approval for all outside collaborators" (checked through the API when it can, else confirmed with `--fork-approval-confirmed`). A first-time contributor's run then waits for a maintainer. | A maintainer who approves without reading the workflow diff. | Built and tested; the API endpoint is **unverified** |
| Public-repository opt-in | `init` and `apply` refuse a public repository without `--allow-public-runner`. | Whoever passes the flag. | Built and tested |
| No root, no inbound ports | The runner runs as you; everything is outbound. | | Built and tested |

The workflow guard is listed because it is worth having (it catches honest mistakes and stops
jobs being queued for your runner at all), not because it is the control. Do not rely on it
alone.

## Per user, per repository

A runner belongs to one person's machine and one repository. So:

1. **The owner** creates a token, runs `bladerunner init` in a clone of their own repository
   (the token is stored in their Keychain or a `0600` file, never in the repo), and runs
   `bladerunner apply`.
2. **A fork** gets the original's `bladerunner.yaml` with it, but `apply` refuses: the checkout's
   `origin` is the fork, not the repository in the file. The forker runs
   `bladerunner init --force --repository <their fork>` with their own token and trusts only
   themselves (`trusted_actors` defaults to the repository owner). The original owner's runner
   is never used for a fork's jobs: a fork's workflows run on the fork's own runners.
3. **A contributor** who wants local runs does the same in their own fork. Nothing they do can
   start a job on the owner's machine.
4. **On the repository** each owner should also set Settings > Actions > General > "Fork pull
   request workflows from outside collaborators" to "Require approval for all outside
   collaborators", and keep the list of people with write access short: a collaborator with
   write access can push a branch, and only the hook stops their job reaching your runner.

## What is assumed and not yet verified

These are the assumptions the enforcement stands on. BR-0 must confirm each on a real machine
and a real repository; see [decision 0005](decisions/0005-only-your-code-runs-here.md).

- The runner honors `ACTIONS_RUNNER_HOOK_JOB_STARTED` from its `.env` file, runs the hook
  before any step, and fails the job if the hook exits non-zero.
- The hook's environment carries `GITHUB_REPOSITORY`, `GITHUB_ACTOR`, `GITHUB_EVENT_NAME`, and
  (for pull requests) `GITHUB_EVENT_PATH` with the event payload. The hook refuses when any is
  missing, so a wrong assumption here blocks your own jobs rather than letting others in.
- `GITHUB_ACTOR` is the person who caused the run, and `GITHUB_TRIGGERING_ACTOR` the person who
  re-ran it.
- The fork-approval endpoint and its `approval_policy` values.

## Network access for isolated jobs

An isolated job gets no network (`none`, the default). A job that must reach GitHub uses the
`allowlist` mode instead of `bridge`: it runs on a network with no route out and no address for
the host, and reaches only the hostnames you list, through a proxy that also refuses any name that
resolves to a loopback, private, link-local or metadata address. The container is audited before it
starts and is refused if its network is anything else. Built and tested on Linux with real Docker,
**not yet verified on macOS**, and not yet started by anything. The allowed hosts can still receive
whatever the job sends them. See [decision 0008](decisions/0008-egress.md).

## What this does not protect against

- **A trusted actor's own code is trusted.** It runs as you, with your files, your Keychain and
  your network. Host isolation (no VM) is a v1 choice. Trust only yourself.
- **A compromised trusted account.** Use two-factor authentication on it.
- **Someone with access to your machine** can edit the hook and its policy.
- **A job that is already running** is not stopped if you later change the policy.
- **Jobs on GitHub-hosted runners** never reach your machine and are not affected.
