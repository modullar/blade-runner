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
   not a public unicast address (loopback, private, link-local including metadata, carrier-grade
   NAT, multicast, reserved, documentation, and IPv6 forms that wrap an IPv4 address), and
   connects to the address it checked, not to the name. A second check runs on the address the
   socket really connects to. This is what makes DNS rebinding irrelevant: a later, different
   answer is never asked for. The address policy has no configuration in the production binary.
4. **The audit** (`internal/isolation`, network mode `allowlist`). After `docker create` and
   before `docker start` the job is refused (BR-E082) unless: its network mode is the egress
   network and it is on no other; it has no extra hosts, links or DNS servers; its proxy
   settings are exactly the ones Blade Runner wrote, with an empty `NO_PROXY` and no other proxy
   variable; and the **network** reads back as internal, a bridge, IPv4 only, with
   `inhibit_ipv4` set, no gateway address, no member but the proxy and the job, and the proxy
   running at the address the job was told. The proxy container is held to the container
   hardening audit too, and must be on exactly its two networks. A caller cannot set proxy
   variables (`Spec.Validate` reserves them).
5. **Lifecycle** (`egress.Manager` / `Session`): `Open` builds the network and proxy and waits
   until the proxy answers; `Apply` fills in the `isolation.Spec`; `Close` removes both
   (idempotent); `Sweep` removes what a crash left behind; `Decisions` reads back what the proxy
   allowed and refused.

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
- **Proxy logs** record host, port, client address and the decision for each request, kept by
  Docker's `local` log driver (1 MiB, 2 files) until `Close` removes the container.
- **Concurrent runs share names.** `RemoveStale` acts on every container with the job label, so two
  test or runner processes on one daemon can remove each other's jobs. The repo's Docker tests take
  a file lock to avoid that between `internal/isolation` and `internal/egress`; other processes
  are not covered.
- **Error codes.** The codes asked for were BR-E080 to BR-E089, but BR-E080 was already the state
  file code, so this uses BR-E081 (invalid allowlist), BR-E082 (egress network could not be built
  or failed its audit) and BR-E083 (the proxy refused a request).
