package supervisor

import (
	"context"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/isolation"
)

// LabelSupervisor marks every container this supervisor starts with its owner's name (the
// runner name), so start-up cleanup removes only what THIS supervisor created. Cleaning up by
// isolation.LabelJob alone would remove any container the isolation package made for anything
// else on the machine, including another supervisor's running job.
const LabelSupervisor = "bladerunner.supervisor"

// DockerRuntime is the Runtime backed by a real Docker daemon: jobs run through
// isolation.Docker (confined, audited, removed afterwards), and stale-container cleanup is scoped
// to this supervisor's own containers.
type DockerRuntime struct {
	Docker *isolation.Docker
	// Owner is the value of LabelSupervisor this runtime cleans up: the runner name.
	Owner string
}

// Run delegates to isolation.Docker.
func (d *DockerRuntime) Run(ctx context.Context, spec isolation.Spec) (isolation.Result, error) {
	return d.Docker.Run(ctx, spec)
}

// RemoveStale removes every container labelled as this supervisor's, running or not: after a
// crash one may still be running with a runner that holds a registration. It returns how many.
func (d *DockerRuntime) RemoveStale(ctx context.Context) (int, error) {
	bin := d.Docker.Bin
	if bin == "" {
		bin = "docker"
	}
	res, err := d.Docker.Exec.Run(ctx, execx.Cmd{Name: bin, Args: []string{"ps", "-aq", "--filter", "label=" + LabelSupervisor + "=" + d.Owner}})
	if err != nil {
		return 0, diag.Wrap(err, diag.CodeIsolation, "cannot list leftover runner containers", "Docker is not answering", "check Docker")
	}
	ids := strings.Fields(res.Stdout)
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := d.Docker.Exec.Run(ctx, execx.Cmd{Name: bin, Args: append([]string{"rm", "-f", "-v"}, ids...)}); err != nil {
		return 0, diag.Wrap(err, diag.CodeIsolation, "cannot remove leftover runner containers", "Docker refused", "run `docker rm -f` on them by hand")
	}
	return len(ids), nil
}
