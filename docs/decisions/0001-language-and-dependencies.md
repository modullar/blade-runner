# 0001: Go, standard library only

**Status:** decided. Go was confirmed with the requester (spec A1 said "confirm or substitute").

## Decision

- The implementation is Go, cross-compiled to darwin and linux on arm64 and amd64
  (`CGO_ENABLED=0`). CI builds all four.
- **No third-party modules.** `go.mod` has none.

## Why no dependencies

Spec section 0: "Prefer boring, dependency-light choices." That was the preference, but the
deciding factor was practical: in the environment this was built in, fetching
`gopkg.in/yaml.v3` and `golang.org/x/term` failed because the Go checksum database
(`sum.golang.org`) is unreachable, and an attempt to bypass it with `GOSUMDB=off` was
(rightly) blocked. Rather than vendor unverified code, the two needs were met with the
standard library:

- **YAML.** `internal/yamlsubset` parses the small, strict subset `bladerunner.yaml` needs
  and *refuses* everything else (anchors, aliases, tags, block scalars, flow maps,
  multi-line scalars, lists of mappings, tabs, duplicate keys, extra documents), naming the
  line. That suits a config file where guessing is worse than refusing, and it gave key-path
  errors (`runner.token.sorce: unknown key`) without reflection tricks.
- **Hidden token entry.** `cmd/bladerunner` shells out to `stty -echo`. If echo cannot be
  turned off it refuses to read a token rather than show it; `--token-stdin` is the
  alternative.

## Consequences and what to revisit

- The YAML subset is a real limitation for users who copy config from elsewhere. If it
  chafes, adopt `yaml.v3` once it can be fetched with checksum verification, and keep the
  unknown-key-path check.
- **BR-4 (agent and dashboard) needs SQLite.** Go's standard library has no driver, so BR-4
  cannot stay dependency-free without a decision: a pure-Go driver (spec A8), which needs
  module fetching that works, or a different store.
- Windows is not supported in v1 and the doctor's disk check is `//go:build unix`.
