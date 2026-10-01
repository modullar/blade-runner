# 0004: Where the implementation departs from, or extends, the spec

Each entry says what the spec said, what was done instead, and why, so a reviewer can
reverse any of them.

## Host isolation with a registration token, not the forge VM design

The forge CI runner uses a Lima VM, single-use just-in-time runners and a fresh container per
job. Blade Runner follows the spec instead: host isolation and a registration token. The forge
code was used for ideas only (see the inventory). Ideas carried over:

- macOS: wait until launchd has really unloaded a service before bootstrapping it again
  (`platform/macos`); forge hit "Bootstrap failed: 5: Input/output error" without it.
- State is a progress log, never the source of truth; every step re-reads the machine.
- Refuse to guess about a public repository.

## Layout per runner name

Everything for runner `NAME` lives under `~/.bladerunner/runners/NAME/` (runner, logs,
downloads, `state.json`), and its token under `~/.bladerunner/secrets/NAME.token`. The spec
showed a single `~/.bladerunner/work`. Per-name paths let two runners share a machine (the
spec's own side-by-side migration needs that) and make `remove` delete one directory.

## Default `work_dir` is per runner

The spec's example is `~/.bladerunner/work`; the default here is
`~/.bladerunner/work/<name>`, for the same reason. An explicit value is always honored.
`remove` deletes a work directory only if it holds a marker file naming this runner, and
never `/` or the user's home.

## The public-repository guard is stricter than the spec

The spec says to refuse `local` or `auto` *placement* on a public repository without an
opt-in. Here `init` and `apply` refuse to register a runner for a public repository at all
without `--allow-public-runner`, whatever the placement: a fork's pull request can change
`runs-on` in its own copy of the workflow, so registering the runner is the exposure. Where
visibility cannot be determined, `apply` fails closed (BR-E061); `init` warns and lets `apply`
decide. For organization scope the tool cannot see which repositories may use the runner, so
it prints a note about runner groups instead. `doctor` reports a runner on a public repository
as a failure.

## `bladerunner.yaml` is not marked "generated, do not edit"

Spec section 0 says every generated file carries a do-not-edit header. The config is generated
once by `init` but is the *source of truth* users are meant to edit, so its header says that
instead. The launchd plist and systemd unit, which really are generated, carry the do-not-edit
header, and a test checks every template does.

## Packages live under `internal/`

The spec's layout uses `internal/`, so the code is a CLI with importable-by-nobody packages.
If the intent was a *library* other tools import, packages such as `core`, `provider`,
`platform` and `config` would move out of `internal/` (or under `pkg/`). That is a rename, not
a redesign, and is worth deciding before anything depends on it.

## Additions the spec did not list

- `BR-E003` (config already exists), `init --force`, `remove --skip-deregister` (for a machine
  that cannot reach GitHub) and `apply` verifying registration against GitHub's label list.
- A directory lock (`BR-E004`) so two `apply` or `remove` runs cannot interleave (`flock`, released
  by the OS if the process dies, so it cannot go stale and leaves no file). Dry-run is not locked.
- Downloads refuse a redirect that leaves https: GitHub serves release assets through a redirect,
  so checking only the first URL would not hold the "https only" rule.
- `testrig` and `githubtest`: a real in-process fake GitHub and a shared test environment, so
  tests run the real client, real files and the real `config.sh` process. Only the service
  manager is faked, because launchd and systemd cannot run inside a test.

## Not built yet (deliberately out of this scope)

`generate`, the JSON Schema, the preflight job and `placement` resolution (BR-3); `status`,
`logs` command wiring, the agent and dashboard (BR-4; the platform `Logs` method exists);
`upgrade`, version pinning beyond the recorded runner version, signed releases (BR-6, BR-7).
