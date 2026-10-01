package isolation

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// egressPrefix marks a violation of the allowlist network design (as opposed to the container
// hardening), so the refusal carries the egress code (BR-E082).
const egressPrefix = "egress: "

// Labels egress.Open puts on what it creates (the proxy container and the network). They are how
// the audit tells the proxy Blade Runner built from any container that merely has its name.
const (
	LabelEgress       = "bladerunner.egress"
	LabelEgressID     = "bladerunner.egress.id"
	EgressRoleSession = "session" // the value of LabelEgress on a session's proxy and network
)

// OptInhibitIPv4 is the bridge option that gives the host no address on the network. Without it
// an internal network still lets its containers reach the host through the bridge's own address
// (observed on Docker 29.3.1, see docs/decisions/0008-egress.md).
const OptInhibitIPv4 = "com.docker.network.bridge.inhibit_ipv4"

// auditEgressContainer checks what a NetworkAllowlist container must look like from its own
// record: on exactly one network, the internal one it was given, with no side door to the host
// (extra hosts, links, DNS servers) and with the proxy settings Blade Runner wrote and nothing
// that exempts a host from them.
func auditEgressContainer(c inspected, want Spec) []string {
	var v []string
	add := func(format string, args ...any) { v = append(v, egressPrefix+fmt.Sprintf(format, args...)) }
	h := c.HostConfig
	if want.EgressNetwork == "" || h.NetworkMode != want.EgressNetwork {
		add("network mode is %q, not the egress network %q", h.NetworkMode, want.EgressNetwork)
	}
	if len(c.NetworkSettings.Networks) != 1 {
		names := make([]string, 0, len(c.NetworkSettings.Networks))
		for n := range c.NetworkSettings.Networks {
			names = append(names, n)
		}
		sort.Strings(names)
		add("is attached to %d networks %v, not only the egress network", len(names), names)
	}
	if _, ok := c.NetworkSettings.Networks[want.EgressNetwork]; !ok {
		add("is not attached to the egress network %q", want.EgressNetwork)
	}
	if len(h.ExtraHosts) > 0 {
		add("has extra host entries %v (a name can be pointed anywhere, the host included)", h.ExtraHosts)
	}
	if len(h.Links) > 0 {
		add("has container links %v", h.Links)
	}
	if len(h.DNS) > 0 {
		add("has its own DNS servers %v", h.DNS)
	}
	env := map[string]string{}
	for _, kv := range c.Config.Env {
		if k, val, ok := strings.Cut(kv, "="); ok {
			env[k] = val
		}
	}
	expected := map[string]string{}
	for _, kv := range ProxyEnv(want) {
		k, val, _ := strings.Cut(kv, "=")
		expected[k] = val
		if got, ok := env[k]; !ok || got != val {
			add("proxy setting %s is %q, want %q", k, got, val)
		}
	}
	for k := range env { // e.g. an ALL_PROXY baked into the image
		if _, ok := expected[k]; !ok && isProxyEnv(k) {
			add("has a proxy setting %s that Blade Runner did not write", k)
		}
	}
	return v
}

// network is the part of `docker network inspect` the audit reads.
type network struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Internal   bool   `json:"Internal"`
	EnableIPv6 bool   `json:"EnableIPv6"`
	Ingress    bool   `json:"Ingress"`
	ConfigOnly bool   `json:"ConfigOnly"`
	IPAM       struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
}

// AuditNetwork reads `docker network inspect` output for the egress network and returns every
// way it is not the network the design needs: internal (no route out), a bridge, IPv4 only, no
// address for the host on it, and no RUNNING member other than the proxy and the job itself.
// The proxy must be running (it is a member) and must be at the address the job was told.
// It also requires the egress manager's label on the network.
//
// Limit: network inspect lists only running containers. A container that was created on the
// network but not started is invisible here; AuditMembers (from `docker ps -a`) covers it.
func AuditNetwork(inspectJSON []byte, want Spec) ([]string, error) {
	var list []network
	if err := json.Unmarshal(inspectJSON, &list); err != nil || len(list) != 1 {
		return nil, fmt.Errorf("cannot read the network's configuration back from Docker (expected one object): %v", err)
	}
	n := list[0]
	var v []string
	add := func(format string, args ...any) { v = append(v, egressPrefix+fmt.Sprintf(format, args...)) }
	if n.Name != want.EgressNetwork {
		add("inspected network is %q, not %q", n.Name, want.EgressNetwork)
	}
	if n.Labels[LabelEgress] != EgressRoleSession || n.Labels[LabelEgressID] == "" {
		add("network %s was not made by the egress manager (it lacks the %s label)", n.Name, LabelEgress)
	}
	if !n.Internal {
		add("network %s is not internal: it has a route out that bypasses the proxy", n.Name)
	}
	if n.Driver != "bridge" {
		add("network %s uses driver %q, not bridge", n.Name, n.Driver)
	}
	if n.EnableIPv6 {
		add("network %s has IPv6 enabled: the audit and the proxy address are IPv4 only", n.Name)
	}
	if n.Ingress || n.ConfigOnly {
		add("network %s is an ingress or config-only network", n.Name)
	}
	if n.Options[OptInhibitIPv4] != "true" {
		add("network %s does not set %s: the host would keep an address on it and its services would be reachable", n.Name, OptInhibitIPv4)
	}
	var subnet netip.Prefix
	for _, c := range n.IPAM.Config {
		if c.Gateway != "" {
			add("network %s has gateway %s: the host owns an address on it", n.Name, c.Gateway)
		}
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
			subnet = p
		}
	}
	want4, err := netip.ParseAddrPort(want.EgressProxy)
	if err != nil {
		return nil, fmt.Errorf("the expected proxy address %q is unreadable: %v", want.EgressProxy, err)
	}
	proxyFound := false
	for _, m := range n.Containers {
		switch m.Name {
		case want.EgressProxyContainer:
			proxyFound = true
			ip, err := netip.ParsePrefix(m.IPv4Address)
			if err != nil || ip.Addr() != want4.Addr() {
				add("the proxy %s is at %q, not at %s", m.Name, m.IPv4Address, want.EgressProxy)
			} else if subnet.IsValid() && !subnet.Contains(ip.Addr()) {
				add("the proxy %s is outside the network's subnet %s", m.Name, subnet)
			}
		case want.Name:
		default:
			add("network %s has another member, %s: containers must not share it", n.Name, m.Name)
		}
	}
	if !proxyFound {
		add("the proxy container %s is not running on network %s", want.EgressProxyContainer, n.Name)
	}
	return v, nil
}

// AuditMembers checks the names of EVERY container attached to the egress network, started or
// not (`docker ps -a --filter network=...`): only the proxy and the job itself may be there. This
// is what enforces one job per session, and what sees a container that was created on the
// network and is waiting to be started.
func AuditMembers(names []string, want Spec) []string {
	var v []string
	seen := map[string]bool{}
	for _, raw := range names {
		for _, n := range strings.Split(raw, ",") { // `docker ps` joins a container's names (links add more)
			n = strings.TrimSpace(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if n != want.EgressProxyContainer && n != want.Name {
				v = append(v, egressPrefix+fmt.Sprintf("container %s is attached to the egress network %s (created, possibly not started): a session serves one job and its proxy, nothing else", n, want.EgressNetwork))
			}
		}
	}
	if !seen[want.Name] {
		v = append(v, egressPrefix+fmt.Sprintf("the job container %s is not attached to the egress network %s", want.Name, want.EgressNetwork))
	}
	return v
}

// AuditProxy checks that the container the network lists under the proxy's name IS the proxy
// the egress manager built: the very container the network holds under that name, carrying the
// manager's labels (and the same session id as the network), running, and held to the same
// hardening as a job. Name and address alone are not identity: any container can be given both.
func AuditProxy(proxyInspect, networkInspect []byte, want Spec) ([]string, error) {
	var nets []network
	if err := json.Unmarshal(networkInspect, &nets); err != nil || len(nets) != 1 {
		return nil, fmt.Errorf("cannot read the network's configuration back from Docker (expected one object): %v", err)
	}
	var list []inspected
	if err := json.Unmarshal(proxyInspect, &list); err != nil || len(list) != 1 {
		return nil, fmt.Errorf("cannot read the proxy's configuration back from Docker (expected one object): %v", err)
	}
	n, p := nets[0], list[0]
	var v []string
	add := func(format string, args ...any) {
		v = append(v, egressPrefix+"proxy "+want.EgressProxyContainer+": "+fmt.Sprintf(format, args...))
	}
	memberID := ""
	for id, m := range n.Containers {
		if m.Name == want.EgressProxyContainer {
			memberID = id
		}
	}
	if memberID == "" || p.ID != memberID {
		add("is not the container the network lists under that name (id %q, network has %q)", p.ID, memberID)
	}
	if !p.State.Running {
		add("is not running")
	}
	if p.Config.Labels[LabelEgress] != EgressRoleSession {
		add("lacks the %s label: it was not built by the egress manager", LabelEgress)
	}
	if id := p.Config.Labels[LabelEgressID]; id == "" || id != n.Labels[LabelEgressID] {
		add("belongs to session %q, not the network's session %q", id, n.Labels[LabelEgressID])
	}
	hv, err := Audit(proxyInspect, Spec{Network: NetworkBridge})
	if err != nil {
		return nil, err
	}
	for _, x := range hv {
		add("is not hardened: %s", x)
	}
	return v, nil
}
