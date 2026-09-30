# 0003: Runner registration and release format (spec A3)

**Status: open. Not verified.** The code and the `githubtest` fake agree with each other; that
is all that has been shown.

## Assumptions the code makes

1. **Unattended registration.** `config.sh --unattended --replace --url <url> --name <name>
   --work <dir> [--labels a,b]`, run in the runner directory.
2. **The token goes in the environment, not argv.** The registration token is passed as
   `ACTIONS_RUNNER_INPUT_TOKEN`. The aim is to keep it out of the process table (spec section
   10). If the runner does not read that variable, registration fails and BR-0 must pick
   another route (for example a prompt fed through stdin), because `--token` on the command
   line is exactly what the spec says to avoid where possible.
3. **Labels.** The runner adds `self-hosted`, the OS and the architecture to whatever
   `--labels` says, so only the *extra* labels are passed, and the API is expected to list all
   of them. `apply` compares the full expected set against GitHub's list (case-insensitively),
   which is how it notices label edits and a runner deleted on GitHub.
4. **The `.runner` file.** After registration the runner directory holds `.runner`, a JSON
   file, written with a UTF-8 byte-order mark, with `agentName` and `gitHubUrl`. The
   registration step reads it to decide whether the machine is already registered.
5. **Release format.** `GET /repos/actions/runner/releases/latest` (or `/tags/v<version>`)
   returns `tag_name`, `assets[].name` (`actions-runner-{osx|linux}-{arm64|x64}-<version>.tar.gz`)
   and `assets[].browser_download_url`; the release notes carry the checksum as
   `<!-- BEGIN SHA osx-arm64 -->HEX<!-- END SHA osx-arm64 -->`. If the checksum is missing the
   install is refused (BR-E030): fail closed.
6. **Service start.** The service runs `run.sh` from the runner directory. The runner's own
   installer uses a different wrapper; whether `run.sh` handles self-update restarts the same
   way is part of A6.
7. **Runner dependencies.** On Linux the runner needs system libraries that GitHub's own
   `installdependencies.sh` installs. `apply` does not run it (it needs root). Not handled in
   BR-2; BR-0 must say whether a prerequisite check is enough.

## What the checksum does and does not prove

The checksum comes from the same GitHub release as the archive, so verifying it proves the
download was not corrupted or altered *in transit*, not that GitHub published a trustworthy
file. It is integrity, not authenticity. Stronger verification (a signature) is not part of
v1.

## Why only latest or a pin

`apply` records the installed release in state and re-resolves *that* version if the runner
directory is lost, so a reinstall does not silently move to a newer runner. Moving versions is
the `upgrade` command's job (BR-6), which does not exist yet. GitHub may also stop sending
jobs to a runner that is too far behind the latest release (the forge setup rebuilds its image
within a day of each release for this reason; unverified here). Until `upgrade` exists, a
pinned runner will eventually need a manual `remove` and `apply`.
