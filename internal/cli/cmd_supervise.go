package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/trust"
)

const superviseUsage = `usage: bladerunner supervise [-c FILE] [--once] [--image REF] [--cancel-unadmitted] [--poll DURATION]

Watch the repository's queue. For each waiting job whose labels match this runner, verify that
its head commit is signed by a key in the trust store; only then start ONE ephemeral, isolated
runner container for it, and always remove the container and the runner registration afterwards.
A job that is not admitted starts nothing and is recorded in ~/.bladerunner/audit/supervisor.jsonl.

  --once               do one cycle and exit (for testing, or to run from cron)
  --image REF          the runner image, pinned by content (name@sha256:...); default supervisor.image
  --cancel-unadmitted  ask GitHub to cancel runs that were refused, so they cannot block the queue
  --poll DURATION      time between cycles (default: agent.poll_idle_seconds)

See docs/decisions/0007-supervisor.md for what this does and does not guarantee.
`

// cmdSupervise wires the supervisor to the real machine: the configured repository, the trust
// store, Docker, and the audit log. The work is in internal/supervisor.
func cmdSupervise(ctx context.Context, args []string, d *Deps) error {
	fs := newFlagSet("supervise", d)
	cfgPath := configFlag(fs)
	once := fs.Bool("once", false, "do one cycle and exit")
	image := fs.String("image", "", "the runner image, pinned by content (name@sha256:...)")
	cancel := fs.Bool("cancel-unadmitted", false, "ask GitHub to cancel runs that were refused")
	poll := fs.Duration("poll", 0, "time between cycles")
	if err := parseFlags(fs, args); err != nil {
		fmt.Fprint(d.Stderr, superviseUsage)
		return err
	}
	env, err := loadEnv(d, *cfgPath)
	if err != nil {
		return err
	}
	cfg := env.Cfg
	if cfg.Runner.Scope != "repo" {
		return diag.New(diag.CodeConfigInvalid, "`supervise` works on a single repository (runner.scope: repo)",
			"organization scope is not supported by the supervisor in this version", "use runner.scope: repo with runner.repository: OWNER/REPO")
	}
	img := cfg.Supervisor.Image
	if *image != "" {
		img = *image
	}
	if img == "" {
		return diag.New(diag.CodeConfigInvalid, "no runner image configured",
			"the supervisor starts every job in a container and needs the image, pinned by content",
			"set supervisor.image: name@sha256:<digest> in bladerunner.yaml, or pass --image")
	}

	dock := &isolation.Docker{Exec: d.Exec}
	info, err := dock.Preflight(ctx)
	if err != nil {
		return err
	}
	goarch, err := dockerArch(info.Architecture)
	if err != nil {
		return err
	}

	// One supervisor at a time: two would both reconcile (remove each other's runners) and both
	// start runners into the same queue.
	unlock, err := core.LockDir(filepath.Join(env.Layout.RunnerHome(), "supervisor"))
	if err != nil {
		return err
	}
	defer unlock()

	audit, err := supervisor.OpenAuditLog(filepath.Join(d.bladeHome(), "audit", "supervisor.jsonl"), nil)
	if err != nil {
		return err
	}
	defer audit.Close()

	scfg := supervisor.Config{
		Provider: env.Provider,
		Admitter: &admit.Admitter{Provider: env.Provider, Verifier: &trust.Verifier{Store: env.Trust}},
		Runtime:  &supervisor.DockerRuntime{Docker: dock, Owner: cfg.Runner.Name},
		Audit:    audit,
		Log:      d.Stdout,
		Scope:    env.Scope(),
		// The runner runs in a Linux container whatever the host is, so its labels are Linux and
		// the container architecture, not the host's.
		RunnerName:       cfg.Runner.Name,
		Labels:           cfg.Runner.AllLabels("linux", goarch),
		Image:            img,
		Network:          cfg.Supervisor.Network,
		MemoryMiB:        cfg.Supervisor.MemoryMiB,
		Timeout:          time.Duration(cfg.Supervisor.TimeoutMinutes) * time.Minute,
		CancelUnadmitted: cfg.Supervisor.CancelUnadmitted || *cancel,
		PollInterval:     time.Duration(cfg.Agent.PollIdleSeconds) * time.Second,
	}
	if *poll > 0 {
		scfg.PollInterval = *poll
	}
	sup, err := supervisor.New(scfg)
	if err != nil {
		return err
	}
	if !*once {
		fmt.Fprintf(d.Stdout, "supervising %s (labels %v, image %s); Ctrl-C to stop\n", scfg.Scope.Repository, scfg.Labels, img)
		return sup.Run(ctx)
	}
	if err := sup.Reconcile(ctx); err != nil {
		return err
	}
	out, err := sup.Tick(ctx)
	switch {
	case err != nil:
	case out.Idle:
		fmt.Fprintln(d.Stdout, "nothing waiting for this runner")
	case out.Launched:
		fmt.Fprintf(d.Stdout, "ran run %d job %d in an isolated container\n", out.RunID, out.JobID)
	case out.Withheld:
		fmt.Fprintf(d.Stdout, "nothing started: %d job(s) refused, see above and the audit log\n", len(out.Refused))
	}
	return err
}

// dockerArch maps the architecture Docker reports to Go's name, which config's label rules use.
func dockerArch(a string) (string, error) {
	switch a {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", diag.New(diag.CodeUnsupportedPlatform, fmt.Sprintf("Docker reports the architecture %q", a),
		"v1 supports amd64 and arm64 containers", "run on an amd64 or arm64 Docker host")
}
