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
	Config struct {
		User string `json:"User"`
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
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
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
	return diag.New(diag.CodeIsolation,
		fmt.Sprintf("container %s was NOT started: it is not confined as required", name),
		"Docker applied a different configuration than was asked for:\n    - "+strings.Join(violations, "\n    - "),
		"update Docker, or report this: Blade Runner will not run a job that is less confined than specified")
}
