# Blade Runner

Turn a Mac mini, or any local macOS or Linux machine, into a self-hosted GitHub Actions
runner with one guided CLI. Placeholder name: the spec notes "Blade Runner" is also a film
franchise's title, so check the name before publishing.

**Status: BR-1 and BR-2 (core) are implemented; nothing has run on a real Mac or a real
GitHub repository yet.** Read [What is verified](#what-is-verified) before relying on it.

```
bladerunner init      # write bladerunner.yaml, store the token, refuse public repos
bladerunner apply     # install/update the runner and its service; --dry-run first
bladerunner doctor    # prerequisites, token, registration, service, disk
bladerunner remove    # deregister and delete everything it installed
```

**Direction (partly built): permission is a public key, and jobs run isolated.** A contributor
is allowed to run code here when the owner adds their public key (`bladerunner trust add`); the
host then checks, itself, that a commit is signed by a trusted key, with no reliance on GitHub's
word. Verification is built and tested (`bladerunner trust verify`); running jobs only when it
passes, and running them in isolated containers, are not built yet. See
[decision 0006](docs/decisions/0006-signed-admission-and-isolation.md).

**Only your code runs on your machine (what is in force today).** `apply` installs a *job hook* that the runner runs
before every job and that refuses anyone not in `runner.trusted_actors` (default: the
repository owner), any other repository, any event outsiders can cause, and any pull request
from a fork, and refuses whenever it cannot tell. Each person sets up their own runner for
their own repository: a fork inherits nothing. See [docs/security.md](docs/security.md) for
the layers, what each can and cannot stop, and what is still assumed rather than verified.

Design in one paragraph: `bladerunner.yaml` is the source of truth. `apply` converges the
machine to it and is safe to re-run or resume: every step re-reads the machine rather than
trusting a record. Isolation is the host itself (no VM), the runner runs as you (never root),
the token lives in the Keychain, a `0600` file or the environment and never in the config, in
logs, or on a command line, and the runner download is verified against its published SHA-256
before anything is unpacked. Outbound connections only.

## Use

```sh
go build -o bladerunner ./cmd/bladerunner
./bladerunner init                          # prompts; or pass flags, see -h
./bladerunner apply --dry-run
./bladerunner apply
./bladerunner doctor
```

`init` non-interactively: `bladerunner init --scope repo --repository OWNER/REPO
--token-stdin < token.txt`. A public repository is refused unless you pass
`--allow-public-runner`, because fork pull requests could then run code on your machine.
Every error says what failed, why, the exact fix, and links to
[docs/errors](docs/errors/README.md).

## Layout

```
cmd/bladerunner/        main: wires the CLI to the real machine
internal/cli/           commands (thin: parse, delegate, print)
internal/config/        bladerunner.yaml: strict loading, validation, defaults, rendering
internal/yamlsubset/    the strict YAML subset the config uses (standard library only)
internal/core/          idempotent, resumable step engine; state file; directory lock
internal/hook/          the job-started policy: who may run jobs here (the enforcement)
internal/guard/         workflow scan: defence in depth, not the enforcement
internal/isolation/     hardened, audited, ephemeral containers for jobs (tested on real Docker)
internal/trust/         ed25519 SSH-signature verification of commits; the trust store
internal/admit/         "may this commit run here?": provider bytes checked by internal/trust
internal/install/       the gates and steps behind apply and remove
internal/doctor/        diagnosis
internal/provider/      CI-provider interface; github/ is the v1 implementation
internal/platform/      service-manager interface; macos/ (launchd) and linux/ (systemd)
internal/secrets/       keychain, 0600 file and environment token stores
internal/download/      verified download and safe unpacking
internal/execx/         the one seam to OS commands
internal/testrig/       whole-environment test harness; provider/github/githubtest is a fake GitHub
templates/              the launchd plist and systemd unit, embedded
docs/                   decisions/, errors/, inventory/
```

## Develop

```sh
gofmt -l .                # must print nothing
go vet ./...
go test -race ./...       # hermetic: real files, real processes, real git, a fake GitHub

# Needs a real service manager (launchd on a Mac, a systemd user session on Linux):
go test -tags integration -v -count=1 ./internal/platform/integration
```

The integration test skips, saying why, where there is no service manager. The full BR-0
procedure for a real Mac is in [docs/BR-0-runbook.md](docs/BR-0-runbook.md).

No third-party modules; see [decision 0001](docs/decisions/0001-language-and-dependencies.md).
CI (`.github/workflows/ci.yml`) runs these on Linux and macOS and cross-compiles darwin and
linux for arm64 and amd64.

## What is verified

**Tested, and green:** config parsing and validation (including key-path errors and the
loopback-only rule), the YAML subset, the step engine (convergence, resume after a failure,
dry-run matches the real run, gates stop everything), token stores, download checksum and safe
unpacking, the GitHub client against an in-process fake GitHub, the `apply`, `doctor` and
`remove` pipelines with real files and a real `config.sh` process, and the command line end to
end, including failure paths.

**Not verified, and why it matters:**

- **Nothing has touched real GitHub.** The fake GitHub encodes assumptions about endpoints,
  the release-notes checksum format, the `.runner` file, label handling and how the runner takes
  its registration token. They are listed in
  [decision 0002](docs/decisions/0002-token-scope.md) and
  [0003](docs/decisions/0003-registration-and-release.md), and BR-0 must confirm or correct
  them. The token's minimum scope is unknown.
- **`launchctl`, `systemctl`, `loginctl` and `security` are driven through fakes** in the
  hermetic tests. The macOS and Linux service code has never run against a real launchd,
  systemd user manager or Keychain; `go test -tags integration` is written for exactly that and
  has not yet been run on either.
- **The job hook's enforcement relies on runner behavior nobody has observed yet** (the runner
  honoring `ACTIONS_RUNNER_HOOK_JOB_STARTED` and passing it the job's identity). The hook logic,
  the installed script and its tamper repair are tested by running the real script as a real
  process; whether the real runner calls it is the first thing the BR-0 runbook checks. The spec's BR-2 gate (a clean machine reaches an online runner; re-running changes
  nothing; `remove` leaves nothing) has not been run on real machines.
- **Service containers under host isolation.** The forge CI jobs use `services: postgres`,
  which host isolation may not support. See the
  [inventory](docs/inventory/BR-1-forge-runner-inventory.md#what-forge-gets-from-its-design-that-host-isolation-does-not-give).

## Not built yet

`generate`, the JSON Schema and the Option A preflight job (BR-3); `status`, `logs`, the
agent and the dashboard (BR-4); `upgrade`, version pinning beyond the recorded runner release,
signed releases and installers (BR-6, BR-7). The forge migration (BR-5) is not started.
