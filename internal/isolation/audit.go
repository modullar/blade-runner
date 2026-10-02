package isolation

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
)

// inspected is the part of `docker inspect` the audit reads: the configuration Docker
// actually applied, not the flags we asked for.
type inspected struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Name   string `json:"Name"`
	Config struct {
		User       string            `json:"User"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
		Image      string            `json:"Image"`
		Entrypoint strList           `json:"Entrypoint"`
		Cmd        strList           `json:"Cmd"`
	} `json:"Config"`
	HostConfig struct {
		Privileged      bool              `json:"Privileged"`
		CapAdd          []string          `json:"CapAdd"`
		CapDrop         []string          `json:"CapDrop"`
		ReadonlyRootfs  bool              `json:"ReadonlyRootfs"`
		SecurityOpt     []string          `json:"SecurityOpt"`
		NetworkMode     string            `json:"NetworkMode"`
		PidMode         string            `json:"PidMode"`
		IpcMode         string            `json:"IpcMode"`
		UTSMode         string            `json:"UTSMode"`
		UsernsMode      string            `json:"UsernsMode"`
		CgroupnsMode    string            `json:"CgroupnsMode"`
		Devices         []any             `json:"Devices"`
		Binds           []string          `json:"Binds"`
		VolumesFrom     []string          `json:"VolumesFrom"`
		PidsLimit       *int64            `json:"PidsLimit"`
		Memory          int64             `json:"Memory"`
		NanoCpus        int64             `json:"NanoCpus"`
		PublishAllPorts bool              `json:"PublishAllPorts"`
		PortBindings    map[string]any    `json:"PortBindings"`
		Tmpfs           map[string]string `json:"Tmpfs"`
		ExtraHosts      []string          `json:"ExtraHosts"`
		Links           []string          `json:"Links"`
		DNS             []string          `json:"Dns"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// strList is a JSON array of strings that Docker may also print as one string or null.
type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = strList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// InspectedID is the container id in `docker inspect` output for one container.
func InspectedID(inspectJSON []byte) string {
	var list []inspected
	if json.Unmarshal(inspectJSON, &list) != nil || len(list) != 1 {
		return ""
	}
	return list[0].ID
}

// Audit reads `docker inspect` output for ONE container and returns every way it is less
// confined than required. An empty result means the invariants hold. The job must not start
// otherwise.
func Audit(inspectJSON []byte, want Spec) ([]string, error) {
	var list []inspected
	if err := json.Unmarshal(inspectJSON, &list); err != nil || len(list) != 1 {
		return nil, fmt.Errorf("cannot read the container's configuration back from Docker (expected one object): %v", err)
	}
	c := list[0]
	h := c.HostConfig
	var v []string
	add := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	// Not root: uid 0 inside a container is root over anything it can reach.
	switch u := strings.TrimSpace(c.Config.User); {
	case u == "", u == "0", u == "root", strings.HasPrefix(u, "0:"), strings.HasPrefix(u, "root:"):
		add("runs as root (user %q)", u)
	}
	if h.Privileged {
		add("is privileged: it has every capability and every device")
	}
	if len(h.CapAdd) > 0 {
		add("adds capabilities %v", h.CapAdd)
	}
	if !contains(h.CapDrop, "ALL") {
		add("does not drop all capabilities (dropped: %v)", h.CapDrop)
	}
	if !h.ReadonlyRootfs {
		add("has a writable root filesystem")
	}
	if !contains(h.SecurityOpt, "no-new-privileges") && !contains(h.SecurityOpt, "no-new-privileges:true") {
		add("can gain privileges (no-new-privileges is not set)")
	}
	for _, o := range h.SecurityOpt {
		if strings.Contains(o, "unconfined") {
			add("disables a security profile (%s)", o)
		}
	}
	for name, mode := range map[string]string{"pid": h.PidMode, "ipc": h.IpcMode, "uts": h.UTSMode, "user namespace": h.UsernsMode} {
		if mode == "host" {
			add("shares the host's %s namespace", name)
		}
	}
	if strings.HasPrefix(h.NetworkMode, "host") || strings.HasPrefix(h.NetworkMode, "container:") {
		add("uses network mode %q, which shares another network stack", h.NetworkMode)
	}
	if want.Network == NetworkNone && h.NetworkMode != "none" {
		add("was asked for no network but has network mode %q", h.NetworkMode)
	}
	if want.Network == NetworkAllowlist {
		v = append(v, auditEgressContainer(c, want)...)
	}
	if len(h.Devices) > 0 {
		add("has host devices attached")
	}
	if len(h.Binds) > 0 || len(h.VolumesFrom) > 0 {
		add("mounts host paths or other containers' volumes")
	}
	for _, m := range c.Mounts {
		if m.Type == "bind" || m.Type == "volume" {
			add("has a %s mount %s -> %s", m.Type, m.Source, m.Destination)
		}
	}
	if h.PublishAllPorts || len(h.PortBindings) > 0 {
		add("publishes ports on the host")
	}
	if h.PidsLimit == nil || *h.PidsLimit <= 0 {
		add("has no process limit (a fork bomb would not stop)")
	}
	if h.Memory <= 0 {
		add("has no memory limit")
	}
	if h.NanoCpus <= 0 {
		add("has no CPU limit")
	}
	return v, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// AuditError turns violations into the refusal shown to the user.
func AuditError(name string, violations []string) error {
	if len(violations) == 0 {
		return nil
	}
	code := diag.CodeIsolation
	for _, x := range violations {
		if strings.HasPrefix(x, egressPrefix) {
			code = diag.CodeEgressSetup // the network design, not the container hardening, failed
			break
		}
	}
	return diag.New(code,
		fmt.Sprintf("container %s was NOT started: it is not confined as required", name),
		"Docker applied a different configuration than was asked for:\n    - "+strings.Join(violations, "\n    - "),
		"update Docker, or report this: Blade Runner will not run a job that is less confined than specified")
}

// RanAuditError is the refusal for a job that DID run: the audit that repeats while it runs (or
// once more when it ends) found the network no longer the design. Its result is not to be
// trusted, and it must not be reported as a job that never started.
func RanAuditError(name string, violations []string) error {
	if len(violations) == 0 {
		return nil
	}
	code := diag.CodeIsolation
	for _, x := range violations {
		if strings.HasPrefix(x, egressPrefix) {
			code = diag.CodeEgressSetup
			break
		}
	}
	return diag.New(code,
		fmt.Sprintf("container %s RAN, but its network was not as required while it ran: its result is not to be trusted", name),
		"the egress topology was checked again while the job ran and when it ended:\n    - "+strings.Join(violations, "\n    - "),
		"discard the result and run the job again; if this repeats, something else on this machine is changing Docker's networks, or report it")
}
