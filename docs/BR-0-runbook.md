# BR-0 runbook: prove it on a real Mac

Everything so far was built where there is no Mac and no route to the live GitHub API. This
runbook is how you (on your Mac, logged in to its desktop) answer the open questions. Paste the
output back, and the answers go into `docs/decisions/`. Nothing here needs more than a few
minutes, and it ends with everything removed.

**Risk.** `modullar/blade-runner` is public. Do step 1 first. The hook is designed to refuse
everyone but you, but it is exactly what this runbook tests, so until step 6 passes treat your
Mac as unprotected: do not leave the runner running.

## 0. Prerequisites

```sh
brew install go          # Go 1.24 or newer
xcode-select --install   # if `git --version` does not work
git clone https://github.com/modullar/blade-runner && cd blade-runner
git checkout claude/optimistic-lovelace-kzj98s
```

## 1. Lock the repository down first

GitHub > blade-runner > Settings > Actions > General:
- **Fork pull request workflows from outside collaborators**: "Require approval for all outside collaborators".
- Settings > Collaborators: nobody but you with write access.

## 2. Run the test suites for real

```sh
go test -count=1 ./...                                   # the ordinary suite
go test -tags integration -v -count=1 ./internal/platform/integration   # REAL launchd
```

The second one installs a harmless service (a script that sleeps) under a unique name, starts,
stops and reloads it, then uninstalls it. **Expected: PASS.** If it fails or skips, paste the
output; it is the first real test of the macOS code.

## 3. Create a token and initialise

Create a token on GitHub (Settings > Developer settings > Fine-grained tokens, limited to this
repository). Start with **Administration: read and write**; if a step below reports BR-E021,
note which permission fixes it. That answers spec A2.

```sh
go build -o bladerunner ./cmd/bladerunner
pbpaste | ./bladerunner init --scope repo --repository modullar/blade-runner \
  --name mac-br0 --allow-public-runner --token-stdin     # token copied to the clipboard first
```

## 4. A probe workflow

On a new branch (a `push` trigger runs from the branch, so nothing needs to be on `main`):

```sh
git checkout -b br0-probe
mkdir -p .github/workflows
cat > .github/workflows/br0.yml <<'YML'
name: br0
on: [push]
jobs:
  probe:
    runs-on: self-hosted
    if: github.actor == 'modullar'
    steps:
      - run: |
          echo "actor=$GITHUB_ACTOR event=$GITHUB_EVENT_NAME repo=$GITHUB_REPOSITORY"
          uname -a; id
YML
git add .github && git commit -qm "BR-0 probe"
```

## 5. Apply, then check health

```sh
./bladerunner apply --dry-run        # read the plan
./bladerunner apply --allow-public-runner --fork-approval-confirmed
./bladerunner doctor
```

**Record:** did apply reach "done"? Did `doctor` pass everything? (A3, A6, A2.) If `apply` stops,
the error code and message are the finding.

## 6. The test that matters: does the hook enforce?

```sh
git push -u origin br0-probe         # the probe runs on your Mac, as you
```

**Expected:** the `probe` job runs and prints `actor=modullar ...`. The job log's "Set up job"
step should show the hook's line `bladerunner: allowed: modullar's push on modullar/blade-runner`.
That shows H1 to H3.

Now make the same job fail for the *right* reason: change who the hook trusts, by editing its
policy file directly (not the config, and not through `apply`, which would repair it), and push
again.

```sh
P=~/.bladerunner/runners/mac-br0/hooks/policy.json
sed -i '' 's/"modullar"/"someone-else"/' "$P" && cat "$P"
git commit --allow-empty -qm "BR-0: the hook must refuse me now" && git push
```

**Expected:** the job **fails before any step runs**, with `bladerunner: REFUSED this job:
modullar is not a trusted actor ...`. If instead it runs and prints `actor=modullar`, **the hook
is not enforcing (H1 or H2 is false): stop, run step 8, and tell me.** That is the most
important result of the whole runbook.

Then let `apply` repair the tampering (this also tests that it notices and restarts the runner):

```sh
./bladerunner doctor                 # expected: job-hook FAILS (policy out of date)
./bladerunner apply --allow-public-runner --fork-approval-confirmed   # expected: job-hook, service-running
./bladerunner doctor                 # expected: all green
```

Next, a pull request from a branch of the same repository (this tests H4): open a PR from
`br0-probe` into `main`. **Expected:** the probe job runs. If you have a second GitHub account,
fork the repository with it and open a PR from the fork: **Expected:** the run waits for your
approval, and if you approve it, the job is **refused** by the hook (it comes from a fork).

## 7. Record what you saw

| Question | Answer |
|----------|--------|
| A2: which token permission was enough? | |
| A3: did `config.sh` register with the token in the environment? | |
| A4: how long does a job wait when the runner is offline (stop the service with `launchctl bootout gui/$(id -u)/dev.bladerunner.runner.mac-br0`, push, and watch)? | |
| A6: did the LaunchAgent start and survive `launchctl bootout`/`bootstrap`? | |
| H1/H2: was the owner refused when the policy did not trust them? | |
| H3/H4/H5: were the `GITHUB_*` values and the event payload present? | |
| H6: did `apply` read the fork-approval setting, or ask for `--fork-approval-confirmed`? | |

## 8. Clean up

```sh
./bladerunner remove --yes
git checkout main && git branch -D br0-probe && git push origin --delete br0-probe
```

Revoke the token on GitHub. `remove` leaves `bladerunner.yaml` and your branches alone.
