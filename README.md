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
internal/core/          idempotent, resumable step engine; state file
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
go test -race ./...
```

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
- **`launchctl`, `systemctl`, `loginctl` and `security` are driven through fakes.** The macOS
  and Linux service code has never run against a real launchd, systemd user manager or
  Keychain. The spec's BR-2 gate (a clean machine reaches an online runner; re-running changes
  nothing; `remove` leaves nothing) has not been run on real machines.
- **Service containers under host isolation.** The forge CI jobs use `services: postgres`,
  which host isolation may not support. See the
  [inventory](docs/inventory/BR-1-forge-runner-inventory.md#what-forge-gets-from-its-design-that-host-isolation-does-not-give).

## Not built yet

`generate`, the JSON Schema and the Option A preflight job (BR-3); `status`, `logs`, the
agent and the dashboard (BR-4); `upgrade`, version pinning beyond the recorded runner release,
signed releases and installers (BR-6, BR-7). The forge migration (BR-5) is not started.
