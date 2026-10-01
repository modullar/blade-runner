# 0006: Cryptographic admission and isolated runners

**Status:** direction set by the requester; **increment 1 (admission) built and tested,
increments 2 to 4 not built.** This supersedes the login-based hook of
[0005](0005-only-your-code-runs-here.md) once it lands, and reverses the "host isolation"
choice in [0004](0004-deviations-and-choices.md).

## The requirement, as given

- Permission to run code on the machine must be a *technical, cryptographic* mechanism applied
  by the host, not a list of trusted logins.
- Anyone (any contributor) can be granted it, and the architecture must hold for anyone using
  the tool.
- Jobs must run only inside an isolated image that cannot escape onto the host.

## Architecture

Two independent layers, plus a supervisor that joins them:

```
GitHub ── queued job ──▶ SUPERVISOR (host) ──admit?──▶ trust store (public keys, on the host)
                              │  yes: commit is signed by a trusted key, verified locally
                              ▼
                 start ONE ephemeral runner in a fresh isolated container
                 (no host mounts, no host credentials, restricted network, destroyed after)
                              │
                     the job runs inside; nothing it does reaches the host
```

1. **Admission (who may run).** The host verifies, with keys it holds, that the commit a job
   would run carries an SSH signature by a trusted key. A commit's signature covers its tree,
   so it covers every file, the workflow included. Steps, each refusing on failure: the commit
   is signed; the signed bytes hash to the commit id asked for (GitHub cannot substitute other
   code); the ed25519 signature is valid, in git's namespace; the key is in the trust store and
   neither revoked nor expired. No runner is started for a job that is not admitted.
   Permission *is* membership in the store: `bladerunner trust add` grants it (a contributor
   gives only their public key), `bladerunner trust revoke` withdraws it.
2. **Isolation (what the code can reach).** Every admitted job runs in a fresh, unprivileged
   container that is destroyed afterwards. Isolation protects the host from *trusted-but-wrong*
   code and from any flaw in admission; admission protects the repository, its secrets and
   your compute from code nobody vouched for. Neither replaces the other.
3. **Supervisor.** Polls for queued jobs, runs admission, starts the isolated runner with a
   just-in-time registration, and binds the runner to the admitted job (the runner's own
   job-started hook refuses any other job it is handed).

### Pull requests

For a pull request the commit to verify is the pull request's **head** commit, never GitHub's
synthetic merge commit (nobody signed that). A contributor from a fork is admitted exactly when
they signed the head commit with a key the owner trusts; the fork itself confers nothing.

## What a signature does and does not prove

It proves a key holder vouched for exactly this tree. It does **not** prove the code is safe or
that the holder's machine was not compromised. That is why isolation is a separate layer, and
why a revoked key is refused at once and forever.

## Status

| Increment | State |
|-----------|-------|
| 1. Admission: SSH signature verification, trust store, `bladerunner trust add/list/revoke/verify`, provider `Commit` | **Built and tested** against real `git` and `ssh-keygen` output and a fake GitHub |
| 2. Isolation runtime: hardened ephemeral containers (`internal/isolation`) | **Built and tested against a real Docker daemon**; not yet connected to anything that starts jobs |
| 3. Supervisor: polling, admission, just-in-time runner, job binding | Not built |
| 4. Retire the host-installed runner, `trusted_actors`, the login-based hook and the workflow guard's actor rule | Not built |

### What the real-Docker tests showed (increment 2)

Run against Docker 29.3.1 (runc, builtin seccomp profile). A probe program inside the container
tried each of these; **blocked** means the kernel refused it:

| A hostile job tries to | Result |
|------------------------|--------|
| write the root filesystem | blocked (read-only) |
| execute a file it wrote to `/tmp` | blocked (`noexec`) |
| `mount`, `chroot`, raw sockets, become root, create a user namespace | blocked (no capabilities, seccomp, no-new-privileges) |
| read a host file (`/etc/shadow`, a file in the host's temp dir) | blocked (nothing is mounted) |
| reach a host service, network `none` | blocked |
| fork without end | stopped by the process limit |
| allocate without end | killed at the memory limit (exit 137, `OOMKilled`) |
| run forever | killed at the timeout; container removed |
| print without end | output capped at 4 MiB per stream |
| **reach a host service, network `bridge`** | **allowed: a known limit** (below) |

The container is created, read back with `docker inspect`, audited, and started only if it is as
confined as specified; a runtime that ignores a flag gets no job (tested by making the real
Docker CLI drop `--read-only`). Every run leaves no container behind, including on timeout and on
refusal. The real test also found, and this increment fixed, a bug the mocks could not: the job
could not write to its own work area because the tmpfs was root-owned.

**Known limit, recorded as a test:** `bridge` network mode reaches whatever the host network
reaches, including services on the host and the LAN. Blocking that needs firewall rules inside
the Docker host (on a Mac, inside its VM), which this package does not install. Until they
exist, a job that needs no network must use `none`, and a runner that must reach GitHub should be
treated as able to reach the host's network.

Until 3 and 4 land, **the trust store is not yet consulted when a job runs**: `apply` still
installs the login-based hook of 0005. `trust verify` only reports what admission would decide.

## Assumptions to verify in BR-0

`bladerunner probe github --repository OWNER/REPO` checks C1, C2 and C3 (and A2, H6) against the
real GitHub and prints a report with no secrets; see [the runbook](../BR-0-runbook.md).

| # | Assumption | If wrong |
|---|------------|----------|
| C1 | `GET /repos/{r}/git/commits/{sha}` returns `verification.payload` and `verification.signature` for a signed commit even when GitHub cannot verify it itself | Admission cannot fetch the signed bytes that way; the fallback is to `git fetch` the object, which needs git and a credential on the host |
| C2 | A just-in-time runner (`generate-jitconfig`) can be started once per job | The supervisor needs another way to bind one runner to one job |
| C3 | A queued job's run exposes the commit to verify (`head_sha`, and for pull requests the head repository) | Admission cannot know which commit a job would run |

## Limits (stated so they are not discovered later)

- **No isolation is absolute.** A container shares its host's kernel; a kernel flaw can let code
  out. A virtual machine is a stronger boundary. On a Mac, Docker always runs containers inside
  a Linux VM, which helps; on Linux the container boundary alone is weaker unless a sandboxing
  runtime such as gVisor is used. The runtime will be as tight as the platform allows and will
  say which boundary it has.
- **Only ed25519 SSH signatures** are supported (what `git commit -S` makes with
  `gpg.format=ssh`). RSA, ECDSA and GPG signatures are refused, not guessed at.
- **The commit id is SHA-1** (GitHub's). Recomputing it binds the signature to the bytes up to
  git's SHA-1 collision hardening; this is git's own trust model, not a new weakness.
- **A trusted signer's code is trusted to run in the container**, with whatever secrets the job
  is given. Give jobs only the secrets they need.
