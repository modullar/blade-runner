# BR-0 runbook: prove the assumptions on a real Mac

Everything so far was built where there is no Mac and no route to the live GitHub API. This
runbook answers the open questions (ADR 0006, "Assumptions") with real behaviour.

**It is safe by construction.** Nothing here installs a runner, registers a service, or lets any
workflow run on your Mac. `bladerunner probe` only reads from GitHub. The one write is a temporary
just-in-time runner registration that is deleted in the same run (skip it with `--skip-jit`).
Do **not** run `bladerunner apply`: until the supervisor exists (ADR 0006, increment 3), nothing
protects a machine that runs a host-installed runner.

## 0. Prerequisites

```sh
brew install go                 # Go 1.24 or newer
brew install --cask docker      # or Colima/OrbStack; `docker version` must work
git clone https://github.com/modullar/blade-runner && cd blade-runner
git checkout claude/optimistic-lovelace-kzj98s
```

## 1. Run the test suites on the real Mac

```sh
go vet ./... && go test -count=1 ./...
```

This includes `./internal/isolation`, which drives the **real Docker** (create, audit, run, remove)
on your machine. **Expected: all `ok`.** A failure in `isolation` is the most useful result: paste
the output, because it means Docker on Mac behaves differently from the Linux that built this.

## 2. Make a signed commit (answers C1)

Admission needs commits signed with an **ed25519 SSH key** (RSA, ECDSA and GPG are refused).

```sh
ssh-keygen -t ed25519 -f ~/.ssh/br_signing -C "blade-runner signing"   # skip if you have one
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/br_signing.pub
git config --global commit.gpgsign true
```

Add `br_signing.pub` on GitHub as a **Signing key** (Settings > SSH and GPG keys), then:

```sh
git checkout -b br0-probe
git commit --allow-empty -m "BR-0: signed commit"
git push -u origin br0-probe
```

GitHub should show "Verified" on that commit.

## 3. Give the probe something to look at (answers C3)

The probe reads run and job records. Trigger one run on GitHub-hosted runners (nothing on your
Mac): add `.github/workflows/br0.yml` on the `br0-probe` branch and push.

```yaml
name: br0
on: [push]
jobs:
  probe:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
```

Wait for it to finish (or leave it queued; either is useful).

## 4. Create a token and run the probe (answers A2, C1, C2, C3, H6)

Create a **fine-grained token** limited to this repository (Settings > Developer settings).
Start with **Administration: read and write** and **Actions: read**; if the probe reports which
call was refused (403), note which permission fixes it: that is the answer to A2.

```sh
go build -o bladerunner ./cmd/bladerunner
read -rs BLADERUNNER_TOKEN && export BLADERUNNER_TOKEN    # paste the token, press return
./bladerunner probe github --repository modullar/blade-runner
```

The report contains no secrets, so paste it back as is. Each line is `PASS`, `FAIL`, `SKIP`
or `NOTE`, with the assumption id. The exit status is 1 if anything failed.

Useful variants:

```sh
./bladerunner probe github --repository modullar/blade-runner --skip-jit   # no temporary runner at all
```

## 5. Clean up

```sh
unset BLADERUNNER_TOKEN
git checkout main && git branch -D br0-probe && git push origin --delete br0-probe
```

Revoke the token on GitHub. If the probe ever printed that it could not delete its temporary
runner, remove it under Settings > Actions > Runners.

## 6. What to send back

1. The output of step 1 if anything failed (otherwise just "all ok").
2. The full probe report from step 4.
3. Which token permissions you used.

With these, the supervisor is built on confirmed behaviour instead of assumptions.
