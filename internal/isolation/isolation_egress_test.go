package isolation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

// The audit of network mode "allowlist": the container's own record, and the network's.
// The JSON below has the shape `docker inspect` / `docker network inspect` printed on Docker
// 29.3.1 for the real topology (the real-Docker tests in internal/egress read the real thing).

func allowSpec() Spec {
	return Spec{
		Name: "job-1", Image: "sha256:" + strings.Repeat("a", 64), Network: NetworkAllowlist,
		EgressNetwork: "br-egress-s1-net", EgressProxy: "172.19.0.1:3128", EgressProxyContainer: "br-egress-s1-proxy",
	}
}

const allowCompliant = `[{
  "Config": {"User": "65534:65534", "Env": ["PATH=/bin", "HTTP_PROXY=http://172.19.0.1:3128", "HTTPS_PROXY=http://172.19.0.1:3128",
     "http_proxy=http://172.19.0.1:3128", "https_proxy=http://172.19.0.1:3128", "NO_PROXY=", "no_proxy="]},
  "HostConfig": {
    "Privileged": false, "CapAdd": null, "CapDrop": ["ALL"], "ReadonlyRootfs": true,
    "SecurityOpt": ["no-new-privileges"], "NetworkMode": "br-egress-s1-net",
    "PidMode": "", "IpcMode": "private", "UTSMode": "", "UsernsMode": "",
    "Devices": [], "Binds": null, "VolumesFrom": null, "PidsLimit": 512,
    "Memory": 4294967296, "NanoCpus": 2000000000, "PublishAllPorts": false, "PortBindings": {},
    "ExtraHosts": null, "Links": null, "Dns": null
  },
  "NetworkSettings": {"Networks": {"br-egress-s1-net": {"Gateway": "", "IPAddress": ""}}},
  "Mounts": []
}]`

const networkCompliant = `[{
  "Name": "br-egress-s1-net", "Driver": "bridge", "Internal": true, "EnableIPv6": false,
  "Ingress": false, "ConfigOnly": false,
  "IPAM": {"Driver": "default", "Config": [{"Subnet": "172.19.0.0/16"}]},
  "Options": {"com.docker.network.bridge.inhibit_ipv4": "true"},
  "Labels": {"bladerunner.egress": "session", "bladerunner.egress.id": "s1"},
  "Containers": {"abc": {"Name": "br-egress-s1-proxy", "IPv4Address": "172.19.0.1/16", "IPv6Address": ""}}
}]`

// proxyCompliant is the proxy's own record: the container the network lists as "abc", built by
// the egress manager for session s1 and hardened like a job.
const proxyCompliant = `[{
  "Id": "abc",
  "State": {"Running": true},
  "Config": {"User": "65534:65534", "Env": ["PATH=/bin"],
     "Labels": {"bladerunner.egress": "session", "bladerunner.egress.id": "s1"}},
  "HostConfig": {
    "Privileged": false, "CapAdd": null, "CapDrop": ["ALL"], "ReadonlyRootfs": true,
    "SecurityOpt": ["no-new-privileges"], "NetworkMode": "br-egress-s1-net",
    "PidMode": "", "IpcMode": "private", "UTSMode": "", "UsernsMode": "",
    "Devices": [], "Binds": null, "VolumesFrom": null, "PidsLimit": 128,
    "Memory": 134217728, "NanoCpus": 1000000000, "PublishAllPorts": false, "PortBindings": {}
  },
  "NetworkSettings": {"Networks": {"br-egress-s1-net": {}, "br-egress-out": {}}},
  "Mounts": []
}]`

func mutateJSON(t *testing.T, in string, edit func(m map[string]any)) []byte {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(in), &list); err != nil {
		t.Fatal(err)
	}
	edit(list[0])
	b, _ := json.Marshal(list)
	return b
}

func TestAuditAcceptsACompliantAllowlistContainerAndNetwork(t *testing.T) {
	if v, err := Audit([]byte(allowCompliant), allowSpec()); err != nil || len(v) != 0 {
		t.Fatalf("container violations = %v, err = %v", v, err)
	}
	if v, err := AuditNetwork([]byte(networkCompliant), allowSpec()); err != nil || len(v) != 0 {
		t.Fatalf("network violations = %v, err = %v", v, err)
	}
}

func TestAuditCatchesEveryProxyBypassRouteOnTheContainer(t *testing.T) {
	nets := func(m map[string]any) map[string]any {
		return m["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	}
	env := func(m map[string]any, e ...string) { m["Config"].(map[string]any)["Env"] = e }
	// withEnv is the compliant proxy environment with some values replaced and some added.
	withEnv := func(replace map[string]string, extra ...string) []string {
		out := []string{}
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
			val := "http://172.19.0.1:3128"
			if strings.EqualFold(k, "NO_PROXY") {
				val = ""
			}
			if r, ok := replace[k]; ok {
				val = r
			}
			out = append(out, k+"="+val)
		}
		return append(out, extra...)
	}
	tests := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"default bridge instead of the egress network", func(m map[string]any) { host(m)["NetworkMode"] = "bridge" }, "not the egress network"},
		{"network mode none", func(m map[string]any) { host(m)["NetworkMode"] = "none" }, "not the egress network"},
		{"a second network", func(m map[string]any) { nets(m)["bridge"] = map[string]any{} }, "attached to 2 networks"},
		{"not on the egress network at all", func(m map[string]any) {
			m["NetworkSettings"] = map[string]any{"Networks": map[string]any{"bridge": map[string]any{}}}
		}, "not attached to the egress network"},
		{"extra host entry", func(m map[string]any) { host(m)["ExtraHosts"] = []string{"github.com:172.17.0.1"} }, "extra host entries"},
		{"container link", func(m map[string]any) { host(m)["Links"] = []string{"other:o"} }, "container links"},
		{"own DNS server", func(m map[string]any) { host(m)["Dns"] = []string{"8.8.8.8"} }, "own DNS servers"},
		{"no proxy settings", func(m map[string]any) { env(m, "PATH=/bin") }, "proxy setting HTTPS_PROXY"},
		{"another proxy", func(m map[string]any) { env(m, withEnv(map[string]string{"HTTPS_PROXY": "http://evil:3128"})...) }, "proxy setting HTTPS_PROXY"},
		{"a host exempted by NO_PROXY", func(m map[string]any) { env(m, withEnv(map[string]string{"NO_PROXY": "169.254.169.254"})...) }, "proxy setting NO_PROXY"},
		{"an ALL_PROXY the image carries", func(m map[string]any) { env(m, withEnv(nil, "ALL_PROXY=socks5://evil:1080")...) }, "ALL_PROXY"},
		{"the host network", func(m map[string]any) { host(m)["NetworkMode"] = "host" }, "network mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Audit(mutateJSON(t, allowCompliant, tc.edit), allowSpec())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("violations %v should include one mentioning %q", v, tc.want)
			}
			if e := AuditError("c", v); diag.CodeOf(e) != diag.CodeEgressSetup {
				t.Errorf("an egress violation must be refused with BR-E082, got %q", diag.CodeOf(e))
			}
		})
	}
}

func TestAuditCatchesEveryWayTheNetworkIsNotTheDesign(t *testing.T) {
	cfg := func(m map[string]any) map[string]any {
		return m["IPAM"].(map[string]any)["Config"].([]any)[0].(map[string]any)
	}
	members := func(m map[string]any) map[string]any { return m["Containers"].(map[string]any) }
	tests := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"not internal", func(m map[string]any) { m["Internal"] = false }, "not internal"},
		{"not a bridge", func(m map[string]any) { m["Driver"] = "macvlan" }, "driver"},
		{"IPv6 on", func(m map[string]any) { m["EnableIPv6"] = true }, "IPv6"},
		{"ingress network", func(m map[string]any) { m["Ingress"] = true }, "ingress"},
		{"the host keeps an address (plain --internal)", func(m map[string]any) {
			m["Options"] = map[string]any{}
			cfg(m)["Gateway"] = "172.19.0.1"
		}, "does not set com.docker.network.bridge.inhibit_ipv4"},
		{"a gateway although the option is set", func(m map[string]any) { cfg(m)["Gateway"] = "172.19.0.1" }, "the host owns an address"},
		{"option set to false", func(m map[string]any) {
			m["Options"] = map[string]any{"com.docker.network.bridge.inhibit_ipv4": "false"}
		}, "inhibit_ipv4"},
		{"another container on the network", func(m map[string]any) {
			members(m)["def"] = map[string]any{"Name": "somebody-else", "IPv4Address": "172.19.0.9/16"}
		}, "another member, somebody-else"},
		{"the proxy is not running", func(m map[string]any) { m["Containers"] = map[string]any{} }, "is not running"},
		{"the proxy is at another address", func(m map[string]any) {
			members(m)["abc"] = map[string]any{"Name": "br-egress-s1-proxy", "IPv4Address": "172.19.0.7/16"}
		}, "is at"},
		{"the network is another one", func(m map[string]any) { m["Name"] = "bridge" }, "inspected network"},
		{"the network was not made by the egress manager", func(m map[string]any) { delete(m, "Labels") }, "was not made by the egress manager"},
		{"the network carries only half the labels", func(m map[string]any) {
			m["Labels"] = map[string]any{"bladerunner.egress": "session"}
		}, "was not made by the egress manager"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := AuditNetwork(mutateJSON(t, networkCompliant, tc.edit), allowSpec())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("violations %v should include one mentioning %q", v, tc.want)
			}
		})
	}
	// The job itself may be a member (it is, once started); that is not a violation.
	v, _ := AuditNetwork(mutateJSON(t, networkCompliant, func(m map[string]any) {
		members(m)["j"] = map[string]any{"Name": "job-1", "IPv4Address": "172.19.0.2/16"}
	}), allowSpec())
	if len(v) != 0 {
		t.Errorf("the job as a member: %v", v)
	}
}

func TestAuditNetworkRefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{"not json": "nope", "empty list": "[]", "two objects": "[{},{}]"} {
		if _, err := AuditNetwork([]byte(in), allowSpec()); err == nil {
			t.Errorf("%s: an unreadable network must never be treated as compliant", name)
		}
	}
}

func TestAllowlistSpecValidation(t *testing.T) {
	good, err := allowSpec().Validate()
	if err != nil {
		t.Fatal(err)
	}
	if good.Network != NetworkAllowlist {
		t.Errorf("network = %q", good.Network)
	}
	bad := map[string]func(s *Spec){
		"no egress network":          func(s *Spec) { s.EgressNetwork = "" },
		"a bad network name":         func(s *Spec) { s.EgressNetwork = "a b" },
		"the default bridge":         func(s *Spec) { s.EgressNetwork = "bridge" },
		"the none network":           func(s *Spec) { s.EgressNetwork = "none" },
		"the host network":           func(s *Spec) { s.EgressNetwork = "host" },
		"the default network":        func(s *Spec) { s.EgressNetwork = "default" },
		"a reserved name, any case":  func(s *Spec) { s.EgressNetwork = "Bridge" },
		"the ingress network":        func(s *Spec) { s.EgressNetwork = "ingress" },
		"the gateway bridge":         func(s *Spec) { s.EgressNetwork = "docker_gwbridge" },
		"another container's stack":  func(s *Spec) { s.EgressNetwork = "container:abc" },
		"no proxy container":         func(s *Spec) { s.EgressProxyContainer = "" },
		"no proxy address":           func(s *Spec) { s.EgressProxy = "" },
		"a proxy by name":            func(s *Spec) { s.EgressProxy = "proxy:3128" },
		"a loopback proxy":           func(s *Spec) { s.EgressProxy = "127.0.0.1:3128" },
		"an IPv6 proxy":              func(s *Spec) { s.EgressProxy = "[fd00::1]:3128" },
		"port zero":                  func(s *Spec) { s.EgressProxy = "172.19.0.1:0" },
		"HTTPS_PROXY in env":         func(s *Spec) { s.Env = map[string]string{"HTTPS_PROXY": "http://evil:1"} },
		"a lower-case proxy in env":  func(s *Spec) { s.Env = map[string]string{"https_proxy": "http://evil:1"} },
		"NO_PROXY in env":            func(s *Spec) { s.Env = map[string]string{"NO_PROXY": "169.254.169.254"} },
		"ALL_PROXY in env":           func(s *Spec) { s.Env = map[string]string{"All_Proxy": "socks5://evil:1"} },
		"egress fields on bridge":    func(s *Spec) { s.Network = NetworkBridge },
		"egress fields on none":      func(s *Spec) { s.Network = NetworkNone },
		"egress fields on a default": func(s *Spec) { s.Network = "" },
	}
	for name, edit := range bad {
		s := allowSpec()
		edit(&s)
		if _, err := s.Validate(); diag.CodeOf(err) != diag.CodeIsolation {
			t.Errorf("%s: err = %v, want BR-E068", name, err)
		}
	}
	// Host networking stays impossible, and none stays the default.
	if _, err := (Spec{Name: "j", Image: good.Image, Network: "host"}).Validate(); err == nil {
		t.Error("host networking must stay impossible")
	}
	if d, _ := (Spec{Name: "j", Image: good.Image}).Validate(); d.Network != NetworkNone {
		t.Errorf("the default network is %q, must be none", d.Network)
	}
}

func TestAllowlistCreateArgsJoinTheInternalNetworkWithProxyEnv(t *testing.T) {
	s, err := allowSpec().Validate()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(createArgs(s), " ")
	for _, want := range []string{
		"--network br-egress-s1-net", "--env HTTPS_PROXY=http://172.19.0.1:3128", "--env https_proxy=http://172.19.0.1:3128",
		"--env NO_PROXY= ", "--read-only", "--cap-drop ALL",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args missing %q:\n%s", want, args)
		}
	}
	for _, forbidden := range []string{"--network bridge", "--network host", "--network allowlist", "--add-host", "--dns", "--link", "--publish"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("create args must never contain %q:\n%s", forbidden, args)
		}
	}
	none, _ := Spec{Name: "j", Image: s.Image}.Validate()
	if a := strings.Join(createArgs(none), " "); strings.Contains(a, "PROXY") || !strings.Contains(a, "--network none") {
		t.Errorf("a none job must carry no proxy settings: %s", a)
	}
}

func TestAuditMembersSeesCreatedButUnstartedContainersAndEnforcesOneJob(t *testing.T) {
	spec := allowSpec()
	// The two that belong: the proxy and the job.
	if v := AuditMembers([]string{"br-egress-s1-proxy", "job-1"}, spec); len(v) != 0 {
		t.Errorf("proxy and job: %v", v)
	}
	// `docker ps` joins a container's names with commas.
	if v := AuditMembers([]string{"job-1", "br-egress-s1-proxy"}, spec); len(v) != 0 {
		t.Errorf("order must not matter: %v", v)
	}
	for name, names := range map[string][]string{
		"a second job":                   {"br-egress-s1-proxy", "job-1", "job-2"},
		"an unstarted stranger":          {"br-egress-s1-proxy", "job-1", "stranger"},
		"a linked alias of the job":      {"br-egress-s1-proxy", "job-1,other/job-1"},
		"a stranger listed with a comma": {"br-egress-s1-proxy", "job-1", "x,stranger"},
		"only strangers":                 {"stranger"},
	} {
		v := AuditMembers(names, spec)
		if len(v) == 0 {
			t.Errorf("%s: %v must be refused", name, names)
			continue
		}
		if e := AuditError("job-1", v); diag.CodeOf(e) != diag.CodeEgressSetup {
			t.Errorf("%s: must be refused with BR-E082, got %q", name, diag.CodeOf(e))
		}
	}
	if v := AuditMembers([]string{"br-egress-s1-proxy"}, spec); len(v) == 0 || !strings.Contains(v[0], "job container job-1 is not attached") {
		t.Errorf("a job that is not on the network at all: %v", v)
	}
}

func TestAuditProxyAcceptsTheProxyTheManagerBuilt(t *testing.T) {
	v, err := AuditProxy([]byte(proxyCompliant), []byte(networkCompliant), allowSpec())
	if err != nil || len(v) != 0 {
		t.Fatalf("violations = %v, err = %v", v, err)
	}
}

func TestAuditProxyRefusesAContainerThatOnlyHasTheNameAndAddress(t *testing.T) {
	labels := func(m map[string]any) map[string]any {
		return m["Config"].(map[string]any)["Labels"].(map[string]any)
	}
	tests := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"no labels at all", func(m map[string]any) { m["Config"].(map[string]any)["Labels"] = nil }, "lacks the bladerunner.egress label"},
		{"the role label has another value", func(m map[string]any) { labels(m)["bladerunner.egress"] = "out" }, "lacks the bladerunner.egress label"},
		{"another session's id", func(m map[string]any) { labels(m)["bladerunner.egress.id"] = "s2" }, "belongs to session"},
		{"no session id", func(m map[string]any) { delete(labels(m), "bladerunner.egress.id") }, "belongs to session"},
		{"a different container than the network lists", func(m map[string]any) { m["Id"] = "zzz" }, "is not the container the network lists"},
		{"not running", func(m map[string]any) { m["State"] = map[string]any{"Running": false} }, "is not running"},
		{"not hardened: writable rootfs", func(m map[string]any) { host(m)["ReadonlyRootfs"] = false }, "is not hardened: has a writable root filesystem"},
		{"not hardened: privileged", func(m map[string]any) { host(m)["Privileged"] = true }, "is not hardened: is privileged"},
		{"not hardened: root", func(m map[string]any) { m["Config"].(map[string]any)["User"] = "root" }, "is not hardened: runs as root"},
		{"not hardened: capabilities", func(m map[string]any) { host(m)["CapAdd"] = []string{"NET_ADMIN"} }, "is not hardened: adds capabilities"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := AuditProxy(mutateJSON(t, proxyCompliant, tc.edit), []byte(networkCompliant), allowSpec())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("violations %v should include one mentioning %q", v, tc.want)
			}
			if e := AuditError("c", v); diag.CodeOf(e) != diag.CodeEgressSetup {
				t.Errorf("must be refused with BR-E082, got %q", diag.CodeOf(e))
			}
		})
	}
	for name, in := range map[string]string{"not json": "nope", "empty": "[]"} {
		if _, err := AuditProxy([]byte(in), []byte(networkCompliant), allowSpec()); err == nil {
			t.Errorf("an unreadable proxy record (%s) must never be treated as compliant", name)
		}
		if _, err := AuditProxy([]byte(proxyCompliant), []byte(in), allowSpec()); err == nil {
			t.Errorf("an unreadable network record (%s) must never be treated as compliant", name)
		}
	}
}
