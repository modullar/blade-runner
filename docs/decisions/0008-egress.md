# 0008: Egress policy for jobs that need the network

**Status:** built and tested on Linux with a real Docker daemon (Docker 29.3.1); **not verified
on macOS** (no Mac here), not connected to a supervisor yet (increment 3 of
[0006](0006-signed-admission-and-isolation.md)). Closes the "`bridge` reaches the host" limit
recorded in 0006, for jobs that opt into the new `allowlist` network mode. `none` stays the
default and `host` stays impossible.

## The requirement

A job that needs the network (a runner must reach GitHub) may reach an allowlist of hostnames
and **nothing else**: not the host, the host's LAN or loopback services, cloud metadata
(`169.254.169.254`), or other containers. The allowlist is configurable.

## The design

```
 job container ──(internal network, one per job, host has NO address on it)──▶ proxy container ──▶ br-egress-out ──▶ internet
   no route out                                                              allowlist by name,
   HTTPS_PROXY=http://<proxy ip>:3128                                         refuses internal addresses
```

1. **A per-job Docker network** created with `--internal` **and**
   `--opt com.docker.network.bridge.inhibit_ipv4=true`. `--internal` means no route out.
   `inhibit_ipv4` means the host has no address on the network (see "What the real tests found":
   without it the host stays reachable).
2. **One proxy per job**, in its own hardened container (`internal/egress/cmd/egress-proxy`,
   static Go, `FROM scratch`), attached to that network and to one shared ordinary bridge,
   `br-egress-out`. It listens only on its address inside the job network (never a wildcard), so
   it is not reachable from the outbound side either.
3. **The proxy** (`internal/egress.Proxy`) accepts HTTP `CONNECT` only, to a plain hostname
   (never an IP literal) on an allowed port (default 443). For each request it checks the
   allowlist, resolves the name **once**, refuses the request if **any** resolved address is
   in a refused range. The refused ranges are Go's own classes
   (loopback, private, link-local including metadata, multicast, unspecified) plus a list of
   special-purpose ranges from the IANA registries (carrier-grade NAT, documentation,
   benchmarking, reserved, deprecated site-local and 6to4 relay, and the IPv6 forms that wrap an
   IPv4 address: IPv4-mapped, SIIT, NAT64, 6to4, Teredo). **That is a deny list, not a proof that
   everything else is public**: a range IANA assigns later is allowed until it is added to
   `forbiddenPrefixes`. The proxy
   connects to the address it checked, not to the name. A second check runs on the address the
   socket really connects to. This is what makes DNS rebinding irrelevant: a later, different
   answer is never asked for. The address policy has no configuration in the production binary.
   The name is resolved as an absolute name (trailing dot), so the resolver's search list cannot
   turn an allowlisted `github.com` into `github.com.<search domain>`. That is all the trailing
   dot does: **`/etc/hosts` is still consulted** for an absolute name by the pure-Go resolver
   the static proxy binary uses (checked here: with an entry `10.9.9.9 x.example`, looking up
   `x.example.` returns 10.9.9.9; a cgo/glibc build did not consult it for the absolute form,
   and Go answered `localhost.` from DNS rather than the file, so do not rely on either
   behaviour), so an entry there can answer for an allowlisted name. It cannot widen anything,
   because `AddressPolicy` gates every address the resolver returns, wherever the answer came
   from, and the dial-time check backs it up. On the way in, a client's target may carry one
   trailing dot (`github.com.` is the same host as `github.com`, logged without the dot); two
   (`github.com..`), an empty label, or a dot in an allowlist entry are refused. Every client connection, tunnelled or not, counts against the connection cap, and
   idle ones (kept alive, or tunnels with no traffic) are closed after `IdleTimeout`.
4. **The audit** (`internal/isolation`, network mode `allowlist`). After `docker create` and
   before `docker start` the job is refused (BR-E082) unless: its network mode is the egress
   network and it is on no other; it has no extra hosts, links or DNS servers; its proxy
   settings are exactly the ones Blade Runner wrote, with an empty `NO_PROXY` and no other proxy
   variable; and the **network** reads back as internal, a bridge, IPv4 only, with
   `inhibit_ipv4` set, no gateway address, no member but the proxy and the job, and the proxy
   running at the address the job was told. Docker's built-in network names (`bridge`, `host`,
   `none`, `default`, `container:*`) cannot be named as the egress network. **One job per
   session** is enforced twice: `Session.Apply` refuses a second job, and the audit lists every
   container attached to the network with `docker ps -a` (network inspect does not show a
   container that was created but not started) and refuses any besides the proxy and this job.
   The same audit repeats every 500 ms while the job runs and kills the job, failing the run
   with BR-E082, if an unexpected container appears (a container created and started in the gap
   between two checks can have reached the job for up to that long). A violation that was READ
   kills the job at once; a pass whose docker commands failed (the topology could not be read)
   counts only when it happens twice in a row, because one hiccup of the daemon is not a
   finding but not knowing twice running is. The kill is retried (up to 10 attempts, growing
   pause) until `docker inspect` says the container is no longer running, not fired once. One
   more audit runs after the job exits. A job that was killed or flagged after it started is
   reported as one that **RAN** (BR-E082, "its result is not to be trusted"), never as "NOT
   started"; "NOT started" is only for a refusal before `docker start`, and an audit that
   cannot read the topology before start is BR-E082 too, not BR-E068. The container named as the
   proxy must be **the one `egress.Open` built**: it carries the manager's labels with the same
   session id as the network, is the container the network lists under that name, is running,
   and passes the same hardening audit as a job (a name and an address are not an identity). The
   proxy container is also held to the hardening audit before it starts, and must be on exactly
   its two networks, which the 500 ms loop re-reads (a third network attached to a running proxy
   kills the job). `Session.Apply` carries what `Open` built the proxy from (image, entrypoint,
   command line) in the `Spec`, and the audit requires the running proxy to carry exactly those.
   **Namespace sharing is a blind spot of every list above:** a container started with
   `--network container:<proxy or job>` joins that container's network stack without being
   attached to any network, so `network inspect` and `ps --filter network=` never show it.
   (`docker ps --format {{.Networks}}` prints nothing for it, which is how the audit finds it.)
   Each pass therefore inspects the network mode of every container that shows no network or
   carries a `bladerunner.*` label, and refuses the job if one shares the proxy's or the job's
   stack (named by id, id prefix or name), or if a Blade Runner container joins any other
   container's stack. A caller cannot set proxy variables (`Spec.Validate` reserves them).
5. **Lifecycle** (`egress.Manager` / `Session`): `Open` builds the network and proxy and waits
   until the proxy answers (on failure it removes only what that call created, by Docker id:
   a name collision with another live session fails without touching it); `Apply` fills in the `isolation.Spec`; `Close` removes both
   (idempotent; the proxy is stopped with a grace period first so it can write what it still holds);
   `Sweep` removes what a crash left behind **for this manager's `Owner` only** (a label on every
   proxy and network; two supervisors on one daemon must set different owners, and the default
   owner is shared by every manager that sets none), carries on past a failure and reports all
   of them; `Decisions` asks the proxy to flush and reads back what it allowed and refused.
   A failed `Open` also reclaims what a create call made without ever reporting an id (context
   cancelled, CLI killed after the daemon had done the work): every object carries a label
   unique to that `Open` call, and the failure path removes what carries it and the session
   id, so a live session with the same id is never matched.

## Alternatives, with trade-offs

Stated from what was tested here or from how the mechanisms work; nothing below is measured
beyond the Linux and Docker 29.3.1 runs described later.

| | For | Against |
|---|---|---|
| **Internal network + allowlisting proxy (chosen)** | Decided by name, which is what "github.com" means; the address check happens at connect time, so changing IPs cost nothing; needs no host privileges beyond Docker; enforced by Docker's own network, whose result the audit reads back; the proxy's decisions are a log of what each job asked for; the same shape works wherever Docker's bridge driver does (see macOS); a client that ignores the proxy fails closed (no route) | Clients must use `HTTPS_PROXY`; only HTTPS `CONNECT` (no plain HTTP, SSH, UDP, QUIC); no content inspection, so an allowed host can still receive anything the job sends it; one network (an address-pool slot) and one small container per job; depends on a Docker bridge option (`inhibit_ipv4`) whose effect was verified only on 29.3.1; on `br-egress-out` the proxy itself can reach the host at network level, so the host is protected from the proxy only by its code (the address policy) |
| **Plain `--internal` network + proxy** (the first idea) | Simplest | **Does not close the gap on Docker 29.3.1**: the job could still reach the host's bridge address. Tested, and kept as a test (`TestRealDocker_PlainInternalNetworkStillReachesTheHost_...`) |
| **iptables / nftables rules (`DOCKER-USER`, `INPUT`) on the Docker host** | Kernel-enforced for every protocol with no client configuration; also protects against a flaw in a proxy | Works on addresses, not names: an allowlist of hostnames needs a resolver that refreshes a set, which races with DNS changes and is exactly where rebinding lives; needs root on the host and rules that survive Docker restarts, reboots and firewall managers; traffic to the host itself passes the `INPUT` chain, not the forwarding path, so two chains must agree; rules live outside what `docker inspect` shows, so the audit cannot read them back; Linux only |
| **pf on macOS** | Native firewall | Containers run inside Docker's Linux VM, so the Mac's `pf` sees the VM's traffic, not a container's; rules would have to live inside the VM, and how to install persistent rules there differs per product. **Not evaluated here** |
| **Transparent proxy (redirect + sniff the TLS server name)** | No client configuration | Needs privileges and redirect rules like the iptables option; trusts a name the client states inside the TLS handshake; breaks as handshakes encrypt more |
| **DNS-only filtering** | Trivial | A job that uses an IP address is not filtered at all |

The proxy and the firewall are not exclusive: on Linux, `DOCKER-USER` rules that drop traffic
from `br-egress-out` to private ranges would be a second layer behind the proxy's address
policy. That is **not built**.

## What the real tests found (Docker 29.3.1, Linux, runc, cgroup v1)

Run from inside real `FROM scratch` containers with a static probe (`internal/egress/testdata/probe`):

| From the job | Result |
|---|---|
| through the proxy to an allowed name (a listener on the host standing in for the internet) | reachable, data flows both ways |
| the **same address and port**, directly | blocked (`network is unreachable`); the host service saw no connection |
| the host's own addresses, the default bridge gateway, the outbound network's gateway | blocked (`network is unreachable`) |
| `127.0.0.1:<port>` | refused: that is the container's own loopback, not the host's |
| `169.254.169.254`, `1.1.1.1`, `8.8.8.8` | blocked (`network is unreachable`) |
| another container on Docker's default bridge (listening, verified reachable from the host) | blocked |
| another job's proxy; this job's proxy on its outbound address | blocked |
| a name not on the list; an IP literal; an allowed name on port 22; a plain `http://` request | refused with 403 / 405, each recorded with its reason |
| an allowed name that resolves to loopback (production proxy, `localhost`) | refused, recorded `forbidden-address` |
| resolving an outside name with the container's resolver | fails (`server misbehaving`): no DNS path out was observed |
| default route | none |
| **plain `--internal`, job to the host's bridge address** | **allowed** (the reason for `inhibit_ipv4`) |

Also observed: with `inhibit_ipv4` the first container on the network is given the network's
first address (`.1`), which is the proxy. `gateway_mode_ipv4=isolated` was tried and showed a
network with no gateway too, but it was not tested end to end and is not used.

The refusal paths are real as well: a runtime that put the job on the default bridge, added a
host entry, attached a second network, or changed or exempted a proxy setting gets no job
(BR-E082, container removed); the network audit refused a plain `--internal` network, an ordinary
bridge, the default bridge, a network with an intruder on it, and one whose proxy had stopped.

Mutation checks removed each guard in turn (the allowlist, the address policy at resolution and at
dial time, the single resolution, ports, CONNECT-only, IP literals, each audit rule, `--internal`,
`inhibit_ipv4`, the wildcard listen guard); the survivors are listed in the build report, not here,
because they depend on the code at that moment.

## macOS (Docker Desktop, Colima, OrbStack) versus Linux

- **Same:** the design uses only Docker's own objects (a bridge network, containers), so it does
  not depend on the host's firewall tooling, and the code is the same on both.
- **Different, and what that does to the claim.** On macOS the engine runs inside a Linux VM, so
  "the host" a job could reach is the VM, and the Mac and its LAN lie behind the VM's NAT. With
  no route and no host address on the job network, none of that is reachable directly; what the
  proxy can reach it reaches through the VM, and the address policy refuses private addresses,
  which includes anything the VM maps the Mac to. **I could not run any of this on a Mac.** In
  particular **unverified**: that those products' engines honour `inhibit_ipv4` the way 29.3.1
  on Linux does. The audit reads the option and the empty gateway back from the engine, so a
  product that ignored it would make jobs refuse to start (BR-E082), not run exposed; but the audit
  reads configuration, and only a test on the real product proves behaviour. Run
  `go test ./internal/egress` on a Mac with Docker before relying on it there.
- **Rootless Docker on Linux** uses a different network stack. Not evaluated.
- The iptables option does not carry over to macOS at all (above).

## Limits

- **Wildcards.** `*.example.com` matches names at any depth below `example.com` (`a.example.com`,
  `a.b.example.com`), never the apex. A wildcard over a public or multi-tenant suffix
  (`*.co.uk`, `*.github.io`, `*.herokuapp.com`) is refused, using a small embedded list in
  `publicsuffix.go` that is **not exhaustive** (the Public Suffix List has thousands of entries):
  the operator remains responsible for every wildcard they write. Names that read as numeric
  IPv4 forms (`1.2.3`, `010.1`, `0x7f.1`) are refused as hostnames.
- **The decision log is bounded, not unlimited, and kept current.**
  - *Refusals* are itemised up to a budget **per reason** (default 100 each), then counted in a
    summary line per reason. `forbidden-address` and `ip-literal` are **never** suppressed: they
    are the attempts to reach inside, and an operator must be able to find every one. (They are
    therefore the one unbounded stream; the connection cap and the header timeout bound their
    rate, and the log can still rotate under a sustained flood of them.)
  - *Allowed decisions* are aggregated per `host:port` per 10 s window: the first is written
    at once, and the rest of the window becomes one line with a `count` and the **last**
    decision's time and client. At most 200 distinct pairs get a first line per window (a
    wildcard entry lets a job invent names); the rest are counted under one line. A burst of
    short allowed CONNECTs therefore cannot rotate Docker's 1 MiB x 2 log and evict refusals.
  - *Staleness.* A timer writes pending summaries and aggregates; SIGTERM flushes on the way out
    (`Close` stops the proxy before removing it); `Session.Decisions` sends SIGUSR1 and waits
    for the proxy's acknowledgement line (`flushed`, hidden from the result) before reading.
    A proxy that is already gone is read as it is.
  - *Line size.* Logged host, port and detail are clipped (64, 8, 200 bytes) and `resolved` to
    8 addresses and 256 bytes with a `+N more` marker. `Decisions` reads with no fixed line
    limit that could stop it: an over-long or broken line (log rotation can cut one) is skipped
    and the lines after it are still read.
- **An allowed name is trusted with whatever the job sends it.** The allowlist limits where a job
  can talk, not what it says: a job holding a token could still use `github.com` to send it
  somewhere (a gist, a push to another repository). A wildcard such as
  `*.actions.githubusercontent.com` trusts every name the domain's owner creates.
- **The default allowlist** (`egress.DefaultAllowlist`) is a guess from GitHub's published runner
  requirements, **not verified against a live runner** (BR-0). It is a default, not a fact.
- **Only HTTPS `CONNECT`.** SSH git remotes, plain HTTP, UDP and QUIC do not work; they fail
  closed. Clients must honour `HTTPS_PROXY`; the probe does, and `git`, `curl` and the runner
  were **not** exercised here (no registry, no GitHub).
- **The proxy is the only layer inside the outbound side.** A bug in `internal/egress` that lets
  an internal address through would expose the host to the proxy container. The address policy
  is tested against loopback, private, metadata and wrapped-IPv4 forms, with real sockets, but
  it is one layer.
- **Name resolution is the container's resolver**, ultimately the host's. A poisoned answer that
  points at a public address is not caught; one that points inside is.
- **Capacity.** Each session takes one Docker network (one allocated subnet; Docker's pool is
  finite, the limit was **not measured**) and one container capped at 128 MiB. A caller starting
  many jobs at once should expect Docker to refuse at some count.
- **The proxy image is not built or pinned by anything yet.** The tests build it into a
  `FROM scratch` image; the supervisor (increment 3) must do that and pass the digest. No
  Dockerfile is shipped.
- **Not connected to anything.** Nothing starts jobs yet; `egress.Manager` and
  `isolation.Docker` are the pieces the supervisor will call.
- **Proxy logs** record host, port, client address and the decision for each request (repeated
  allowed ones aggregated, see above), kept by Docker's `local` log driver (1 MiB, 2 files)
  until `Close` removes the container. Nothing persists them past that: a caller that wants the
  record must call `Decisions` first.
- **An idle tunnel** is one with no traffic in **either** direction for `IdleTimeout`: a long
  download to a silent client, or an upload with nothing coming back, stays open.
- **Concurrent runs share names.** `RemoveStale` acts on every container with the job label, so two
  test or runner processes on one daemon can remove each other's jobs. The repo's Docker tests take
  a file lock (`internal/dockerlock`, in a per-user private directory whose owner and mode are
  checked, for the cache directory as well as the temp fallback) to avoid that between
  `internal/isolation` and `internal/egress`; processes of another user, and other test
  packages that do not call it, are not covered. The lock is re-entrant for a test and its
  subtests (it used to deadlock against itself), and on Windows, which has no `flock`, a test
  that asks for it is skipped.
- **Error codes.** The codes asked for were BR-E080 to BR-E089, but BR-E080 was already the state
  file code, so this uses BR-E081 (invalid allowlist), BR-E082 (egress network could not be built
  or failed its audit) and BR-E083 (the proxy refused a request).
