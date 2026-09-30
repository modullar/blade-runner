# 0002: GitHub token scope (spec A2)

**Status: open. Not verified.** This page records what the code assumes so BR-0 can confirm
or correct it. Nothing here was tested against the live GitHub API.

## What the code calls

| Purpose | Request (assumed) |
|---------|-------------------|
| List runners (also the auth check) | `GET /repos/{owner}/{repo}/actions/runners` or `GET /orgs/{org}/actions/runners` |
| Mint a registration token | `POST .../actions/runners/registration-token` |
| Deregister | `DELETE .../actions/runners/{id}` |
| Repository visibility | `GET /repos/{owner}/{repo}` |

All send `Authorization: Bearer <token>` and `X-GitHub-Api-Version: 2022-11-28`. The public
release lookup deliberately sends no token.

## What is not known

- **The minimum scope.** The spec notes that sources disagree: one says organization admin
  permission, another lists a fine-grained "self-hosted runners: read" permission. The forge
  CI setup (`ci/self-hosted/README.md` in `loopforgelab-forge`) uses a fine-grained token with
  *Administration* read and write to mint runner configurations; that is the forge team's
  statement, not something verified here, and it covers a different endpoint (just-in-time
  configs) than the registration token used here.
- Whether the **default `GITHUB_TOKEN`** can list runners. The spec's view is that it
  probably cannot, which is why the BR-3 preflight job needs a separate secret.
- Whether a **narrower, list-only token** is enough for the preflight job (it only reads
  runner status). That matters because that token lives in a repository secret that every
  hosted preflight run can read.

## Until BR-0 answers

- `init` stores whatever token the user supplies and checks it by listing runners, so a token
  that cannot do the job is rejected immediately (BR-E021) rather than halfway through `apply`.
- Error messages point here. Do not treat any permission named above as a recommendation.
- BR-0 must end with a written recommendation: the smallest permission that lets `apply`,
  `remove` and `doctor` work, and the smallest that lets the preflight job list runners.
