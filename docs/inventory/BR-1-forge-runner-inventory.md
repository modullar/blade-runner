# BR-1: Inventory of the runner setup in `loopforgelab-forge`

Spec section 14, step 1: list every runner-related file and classify it as **tool logic**
(moves out), **project config** (stays, shrinks) or **machine state** (belongs to neither).

**Source read:** `main` of `modullar/loopforgelab-forge` at `2d325cf`. Both branches named in
the request, `claude/ci-minute-savings` and `claude/awesome-clarke-5q2i39`, are already
ancestors of `main`, so this is their merged state.

**Direction given:** Blade Runner uses *host isolation* and the *Option A preflight job*. The
forge code is inspiration, not a port. So besides classifying each file, the last column says
what Blade Runner does with it: **ported**, **replaced** (a different mechanism does the same
job), or **not ported** (forge-only behavior the chosen design leaves out).

## Files in the repository

| File | What it is | Class | In Blade Runner |
|------|-----------|-------|-----------------|
| `ci/self-hosted/forge-ci` | Launcher: runs `scripts/forge_ci` under `uv` | tool logic | **replaced** by the `bladerunner` binary |
| `scripts/forge_ci/cli.py` | `install`, `vm-up`, `vm-recreate`, `image-build`, `verify`, `run`, `start`, `stop`, `pause`, `resume`, `failover`, `recover`, `status` | tool logic | **partly ported**: `install` maps to `init` plus `apply`, and `start`/`stop` to `apply`/`remove`; `status` is BR-4; `vm-*`, `image-build`, `verify`, `run`, `pause`, `resume`, `failover` and `recover` are VM, supervisor or failover features (not ported) |
| `scripts/forge_ci/config.py` | TOML settings on the Mac; sizes slots and the VM for the RAM | tool logic | **replaced** by `bladerunner.yaml` (`runner.*`, `agent.*`); slot and VM sizing **not ported** |
| `scripts/forge_ci/host.py` | Runs commands; Keychain token read; memory-pressure reader; machine gate; `caffeinate` | tool logic | **ported**: Keychain read/write (`secrets`), command runner (`execx`), machine gate as prerequisites. Memory-pressure gate and `caffeinate` **not ported** |
| `scripts/forge_ci/launchd.py` | Generates and loads the LaunchAgent; waits for `bootout` to finish | tool logic | **ported** (`platform/macos`), including the wait |
| `scripts/forge_ci/runners.py` | GitHub just-in-time runner API; one-job container argv | tool logic | **replaced**: registration token and a long-lived runner. JIT and per-job containers **not ported** |
| `scripts/forge_ci/vm.py`, `image.py`, `verify.py` | Lima VM, runner image build/rotate, isolation proof (gate V1) | tool logic | **not ported**: VM isolation is a v1 non-goal |
| `scripts/forge_ci/health.py` | Route controller with hysteresis; busy gate | tool logic | **replaced** by the preflight job (BR-3) |
| `scripts/forge_ci/supervisor.py` | Run loop: slots, health, heartbeat, failover, flip back | tool logic | **not ported** (see "What Option A does not cover") |
| `scripts/ci_failover.py` | Flips `FORGE_RUNNER`; rescues, re-queues and recovers stranded runs | tool logic | **replaced** by the preflight job for *new* jobs; rescue of already-queued jobs **not ported** |
| `scripts/check_runner_policy.py` | Tripwire: every job's `runs-on` is the routing expression or an allowed exception | tool logic, project-side | **replaced** by `generate --check` (BR-3); stays in forge until cutover |
| `ci/self-hosted/Dockerfile`, `entrypoint.sh`, `vm-provision.sh`, `lima.yaml`, `daemon.json`, `registry-mirror.yml` | The VM and the runner image | tool logic | **not ported** (VM isolation) |
| `ci/self-hosted/README.md` | Setup and runbook | docs | **split**: the generic parts become Blade Runner's docs; the forge-specific parts (tokens, repo settings) stay with the project |
| `tests/ci_runner/*` (13 files) | Tests for all of the above | tests | move or retire with the code they test |
| `.github/workflows/ci.yml`, `frontend.yml` | Jobs routed by `runs-on: ${{ github.ref != 'refs/heads/main' && vars.FORGE_RUNNER || 'ubuntu-latest' }}` | project config | **generated** from `bladerunner.yaml` placement (BR-3) |
| `.github/workflows/runner-failover.yml` | Hosted workflow that flips the variable and re-queues; also the emergency brake | tool logic | **replaced**; the brake has no equivalent yet (see below) |
| `.github/workflows/geometry-tests.yml`, `step-corpus-survey.yml`, `slow-tests.yml` | Always hosted | project config | become `placement: github` |
| `CLAUDE.md` (CI section) | Explains the runner routing and failover | project docs | shrinks to a pointer at `bladerunner.yaml` |

## Machine and GitHub state (belongs to neither the tool nor the project)

| Item | Where |
|------|-------|
| `~/forge-ci/config.toml`, `state/` (`last-healthy`, `requeue/` journal), `logs/` | the Mac |
| The Lima VM `forge-ci`, its images, the `forge-runner` and `previous` images | the Mac |
| LaunchAgent `com.loopforgelab.forge-ci` | the Mac |
| Keychain item `forge-ci-pat` | the Mac |
| Power settings (`pmset`), automatic-update setting, FileVault | the Mac |
| Repository variables `FORGE_RUNNER`, `FORGE_RUNNER_HOLD` | GitHub |
| Repository secret `FORGE_SWITCH_TOKEN` | GitHub |
| healthchecks.io check `forge-mac-alive` and its webhook | external service |
| Three fine-grained tokens (`forge-ci-mac`, `forge-switch`, `forge-failover-webhook`) | GitHub account |

Blade Runner's own machine state is under `~/.bladerunner/` and is removed by
`bladerunner remove`. It never touches the items above; the forge ones are retired by hand at
cutover.

## Derived `apply` step list (BR-2)

As implemented in `internal/install`. Gates run first and change nothing, dry-run included.

| # | Gate / step | Checks (reads the machine) | Applies |
|---|-------------|----------------------------|---------|
| g1 | `not-root` | effective uid is not 0 | |
| g2 | `prerequisites` | platform prerequisites all pass | |
| g3 | `token-present` | a token is stored | |
| g4 | `token-valid` | GitHub accepts it and it can list runners | |
| g5 | `public-repo-guard` | repository is not public (or `--allow-public-runner`) | |
| 1 | `runner-binary` | `runner/` holds a marker and `config.sh`, `run.sh` | resolve release (pinned if recorded), download, verify SHA-256, unpack beside, rename into place, record the pin |
| 2 | `work-dir` | work dir exists with Blade Runner's marker | create it, write the marker |
| 3 | `runner-registration` | `.runner` matches name and URL, *and* GitHub lists the runner with every expected label | mint a registration token, run `config.sh` with it in the environment |
| 4 | `service-definition` | definition file exists and equals what would be generated | write it; reload if changed |
| 5 | `service-running` | the service is running | start it |

`remove` is the reverse, keeping what later steps need until they have run: stop and uninstall
the service, deregister from GitHub (while the token still exists), delete runner files, delete
the work directory *only if Blade Runner created it*, delete the stored token, then delete the
local state and any now-empty shared directories. It never touches `bladerunner.yaml` or
workflow files.

## What forge gets from its design that host isolation does not give

Flagged for a decision before BR-5; none of this is resolved here.

1. **Service containers.** The two locally routed jobs in `ci.yml`, and the hosted `e2e` job in
   `frontend.yml`, use `services: pgvector/pgvector:pg16`. The forge VM provides that through a
   Docker daemon inside each job's container. *Assumption, not verified:* GitHub's service and
   container features need Docker and a Linux runner, so on a Mac under host isolation those
   jobs would not run as written. This is the most likely blocker to "Forge CI green using only
   Blade Runner" (BR-5), and BR-0 should test it early.
2. **Clean state per job.** Forge runs one job per fresh container. Under host isolation jobs
   share the machine's filesystem, caches and processes, so a job can leave state for the next.
3. **Already-queued jobs.** See below.
4. **Busy-machine step-aside.** Forge hands jobs to GitHub when the Mac is under memory
   pressure. The spec lists busy-runner fallback as a non-goal.
5. **The emergency brake.** Forge can flip every PR job to hosted runners from the Actions tab.
   With Option A, the nearest equivalent is stopping the runner service or removing it.
6. **The spec's BR-5 acceptance says "old runner scripts removed from the repository."** That is
   only coherent once 1 to 5 are decided: either forge accepts losing them, or the VM isolation
   stays in some form.

## What Option A does not cover

Option A picks `runs-on` when a workflow run *starts*. The forge supervisor also rescues jobs
already queued for a runner that then went away (`ci_failover.py rescue`: cancel, then re-run),
because, per that file, a job queued for an offline label waits up to 24 hours before failing.

*My analysis, not verified:* a preflight job cannot help a job it already routed to a runner
that dies afterwards, and the spec scopes busy and mid-run failures out. Whether that matters
depends on A4 (how long a job really waits) and on how often the machine drops between
preflight and pickup. The documented mitigation in the spec is `placement: auto` for
everything that may fall back, and a `doctor` warning when `local` is used.
