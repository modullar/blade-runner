# Blade Runner error codes

Every error `bladerunner` prints has a stable code, says what failed, the likely cause, and
the exact fix, and links here. `internal/diag` declares the codes; a test fails if a code
has no section below.

Codes are grouped: `E00x` config, `E01x` machine, `E02x` token and GitHub, `E03x` runner
download, `E04x` registration, `E05x` service, `E06x` public-repository guard, `E07x`
health, `E08x` state, `E09x` confirmation, `E10x` warnings about placement.

### BR-E001

**The config file is invalid.** `bladerunner.yaml` has an unknown key, a wrong value or a
syntax error. Every problem is listed with its key path (for example
`runner.token.sorce: unknown key (did you mean "source"?)`). Unknown keys are errors on
purpose, so a typo cannot silently fall back to a default. Fix the keys named, then re-run.
The file format is a strict YAML subset: no anchors, aliases, tags, block scalars, flow
maps, multi-line scalars or lists of mappings.

### BR-E002

**There is no config file** at the path given (default `./bladerunner.yaml`). Run
`bladerunner init` in the project, or pass `--config <path>`.

### BR-E003

**`init` will not overwrite an existing config.** Edit the file, or re-run with `--force`
to replace it.

### BR-E004

**Another `bladerunner` command is already changing this machine.** `apply` and `remove`
hold a lock on `~/.bladerunner` (or `$BLADERUNNER_HOME`) so two runs cannot interleave, for
example a scheduled `apply` and one you started by hand. Wait for the other run to finish and
try again. The lock is released automatically if that process dies, so it cannot go stale.

### BR-E010

**A prerequisite is missing.** The message lists each one and its fix: git, Xcode Command
Line Tools and a GUI login session on macOS; git, systemd and a reachable systemd user
manager on Linux. Install or fix what is listed, then re-run.

### BR-E011

**This OS or CPU architecture is not supported.** v1 supports macOS and Linux (systemd), on
arm64 and amd64.

### BR-E012

**`bladerunner` is running as root.** The runner must run as your own user, because a job
that runs as root owns the whole machine. Run it again without `sudo`.

### BR-E020

**No GitHub token is stored for this runner.** Tokens are stored per runner name, outside
`bladerunner.yaml`: in the macOS Keychain, in a `0600` file under `~/.bladerunner/secrets`,
or in the `BLADERUNNER_TOKEN` environment variable, as `runner.token.source` says. Run
`bladerunner init` again (or `bladerunner init --force --token-stdin < token.txt`), or
export `BLADERUNNER_TOKEN` for the env source.

### BR-E021

**GitHub rejected the token, or the token is not allowed to manage runners**, or it cannot
see the repository or organization (GitHub answers 404 to hide private resources). Check the
names in `runner.repository` / `runner.organization`, and create a token with the scope in
[decisions/0002-token-scope.md](../decisions/0002-token-scope.md). The minimum scope is
unconfirmed until BR-0, so that page says what is assumed.

### BR-E022

**GitHub could not be reached, or returned an error**: a network problem, a proxy or
firewall, a rate limit (the message then says when it resets), or a GitHub outage. Re-run
in a minute.

### BR-E023

**The token could not be stored or read.** The file backend refuses a token file readable
by other users (`chmod 600` it); the keychain backend needs the login keychain unlocked; the
env backend cannot store anything; and a token containing whitespace or quotes is refused as
mistyped.

### BR-E030

**The runner release could not be resolved.** GitHub has no build for this OS and
architecture in that release, a pinned version no longer exists, or the release notes do not
carry the SHA-256 in the expected format. Blade Runner refuses to install without a
checksum. See [decisions/0003-registration-and-release.md](../decisions/0003-registration-and-release.md).

### BR-E031

**The runner download's SHA-256 did not match.** The download was discarded and nothing was
installed. Re-run `bladerunner apply`; if it fails again, do not bypass the check: report it.

### BR-E032

**The runner download failed or the archive is unsafe**: a network error, an insecure URL, a
corrupt archive, or an archive entry that would write outside its destination.

### BR-E040

**The runner's configure script failed.** The registration token was refused or expired, the
name or labels were rejected, or the runner's own dependencies are missing. The script's own
message is shown as detail (with the token redacted). A fresh token is minted on every
`apply`, so re-running is safe.

### BR-E041

**The runner is not registered (or is registered with the wrong labels).** `doctor` found no
runner with this name on GitHub, or it lacks the configured labels. Run `bladerunner apply`.

### BR-E050

**The service could not be installed, started or stopped.** On macOS this usually means no
GUI login session (log in to the desktop as this user) or a stuck launchd unload; on Linux,
an unreachable systemd user manager or lingering that needs administrator rights once:
`sudo loginctl enable-linger $USER`. Re-run `bladerunner apply`: it resumes where it stopped.

### BR-E051

**The service is installed but not running.** Run `bladerunner apply` to start it, then
`bladerunner logs` if it stops again.

### BR-E052

**GitHub sees the runner as offline.** The service may be stopped or the machine may have no
network. Check the service (BR-E051) and the logs.

### BR-E060

**A public repository was refused.** Anyone can open a pull request from a fork, and a
self-hosted runner would run their code on this machine. Use GitHub-hosted runners for public
repositories (`placement: github`). If you understand the risk and trust every contributor,
re-run with `--allow-public-runner`. `doctor` reports a runner on a public repository as a
failure.

### BR-E061

**Blade Runner could not tell whether the repository is public**, so it refuses to go on.
Fix the problem reported with it (usually connectivity or token scope) and re-run.

### BR-E062

**A workflow would let someone else's code be *sent* to your machine.** A job that can land on
your runner (`runs-on: self-hosted`, a label of this runner, or an expression that might
resolve to one) is not locked to the people you trust, or its workflow uses a trigger that
outsiders can fire (`pull_request_target`, `issue_comment`, `workflow_run` and the like). The
message lists each job and the exact `if:` line to add:

```yaml
if: github.actor == 'you' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)
```

**This is defence in depth, not the control.** A workflow file is written by whoever opens the
pull request or pushes the branch, so an attacker can delete the `if:` in their own copy. What
actually stops them is the job hook on your machine (BR-E065), which they cannot edit. The
workflow guard still earns its place: it catches honest mistakes, keeps unauthorized jobs from
being queued for your runner at all, and makes the intent reviewable. The trusted logins are
`runner.trusted_actors` (default: the repository owner). The check is textual and strict: a
guard it cannot prove is treated as missing. Jobs that only run on GitHub-hosted images need
nothing. See [docs/security.md](../security.md).

### BR-E063

**This checkout is not the repository the config registers a runner for.** A runner belongs to
one person's machine and one repository. If you forked a project that commits a
`bladerunner.yaml`, run `bladerunner init --force` to set up your own repository with your own
token; the original owner's runner is not yours to use. `--allow-repo-mismatch` overrides it
for the rare intentional case.

### BR-E064

**The fork pull request approval setting of a public repository is not strict, or cannot be
verified.** On a public repository, a first-time contributor's pull request can otherwise start
running at once. Set Settings > Actions > General > "Fork pull request workflows from outside
collaborators" to "Require approval for all outside collaborators". If Blade Runner cannot read
the setting itself (the endpoint it uses is unverified, see
[decision 0005](../decisions/0005-only-your-code-runs-here.md)), confirm you set it with
`--fork-approval-confirmed`.

### BR-E065

**The job hook is missing, out of date, or cannot run.** The hook is what keeps other people's
code off your machine: the runner runs it before every job and a non-zero exit fails the job
before any step starts. It is a generated script plus a policy file under
`~/.bladerunner/runners/<name>/hooks/`, and the runner is told about it through
`ACTIONS_RUNNER_HOOK_JOB_STARTED` in its `.env`. `doctor` reports a missing or edited script
or policy, a missing `.env` entry, or a `bladerunner` program that has moved (the script calls
it, and every job is refused while it is gone). Run `bladerunner apply`: it rewrites what is
wrong and restarts the runner so it picks the hook up.

### BR-E066

**The trust store cannot be read or written.** It is the file of public keys this machine
trusts (`~/.bladerunner/runners/<name>/trust.json`), and its content decides which commits may
run, so it is never guessed at: a malformed file stops everything rather than being treated as
empty. Restore it from a backup, or move it aside and re-add keys with `bladerunner trust add`.
Adding a key that is already trusted, reusing a name, or re-adding a revoked key is refused here
too: a revoked key stays revoked.

### BR-E067

**A commit is not admitted to run on this machine.** This machine runs only commits signed by a
key in its trust store, and it checks that itself: the commit is signed; the signed bytes hash
to the commit id (so they cannot be swapped for others); the signature is valid for the key it
names; and that key is trusted, unexpired and unrevoked. The message says which check failed.
To give someone permission, add their *public* key: `bladerunner trust add --name alice --key
alice.pub`; they sign commits with the matching private key (`git config gpg.format ssh`,
`git commit -S`). To take it back: `bladerunner trust revoke alice`. Only SSH signatures with
ed25519 keys are supported.

### BR-E070

**Low disk space** where the runner works. Below 10 GiB free is a warning and below 2 GiB a
failure; jobs fail confusingly when the work directory fills up. Free space.

### BR-E071

**This `bladerunner` is older than `bladerunner.min_version`.** Upgrade it.

### BR-E080

**The local state file is unreadable, corrupt, or from a newer `bladerunner`.** State is a
progress log, not the source of truth: delete the file named in the message and `apply`
rebuilds it from the machine. A file from a newer version means this CLI must be upgraded.

### BR-E090

**`remove` was not confirmed.** It deletes the runner, its service and its local state, so it
asks you to type the runner name, or to pass `--yes` when no terminal is attached.

### BR-E100

**Some jobs are pinned to the local runner (`placement: local`).** They queue, and wait,
while the runner is down instead of falling back to GitHub-hosted runners. Use
`placement: auto` for jobs that may fall back.
