package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/download"
	"github.com/modullar/blade-runner/internal/execx"
)

// ApplySteps are the convergent steps of `apply`, in order.
func ApplySteps(e *Env) []core.Step {
	return []core.Step{
		&fetchRunner{e}, &workDir{e}, &registerRunner{e}, &jobHook{e}, &serviceDefinition{e}, &serviceRunning{e},
	}
}

const (
	runnerMarker  = ".bladerunner-runner.json" // inside the runner dir: which release it holds
	workDirMarker = ".bladerunner-work"        // inside the work dir: Blade Runner owns it
)

// ---- fetch the runner --------------------------------------------------------------

type fetchRunner struct{ e *Env }

func (*fetchRunner) ID() string       { return "runner-binary" }
func (*fetchRunner) Describe() string { return "download, verify and unpack the GitHub Actions runner" }

type runnerMark struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func (s *fetchRunner) installed() (runnerMark, bool) {
	dir := s.e.Layout.RunnerDir()
	data, err := os.ReadFile(filepath.Join(dir, runnerMarker))
	if err != nil {
		return runnerMark{}, false
	}
	var m runnerMark
	if json.Unmarshal(data, &m) != nil || m.Version == "" {
		return runnerMark{}, false
	}
	for _, f := range []string{"config.sh", "run.sh"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return runnerMark{}, false
		}
	}
	return m, true
}

func (s *fetchRunner) Check(context.Context) (core.Status, error) {
	if m, ok := s.installed(); ok {
		return core.Status{Done: true, Reason: "runner " + m.Version + " installed"}, nil
	}
	return core.Status{Reason: "runner not installed"}, nil
}

func (s *fetchRunner) Apply(ctx context.Context) error {
	e := s.e
	st, err := e.State.Load()
	if err != nil {
		return err
	}
	// A recorded pin wins over "latest": reinstalling must not drift to another release.
	rel, err := e.Provider.Release(ctx, e.GOOS, e.GOARCH, st.RunnerVersion)
	if err != nil {
		return err
	}
	archive := filepath.Join(e.Layout.DownloadDir(), rel.Filename)
	if !download.HasChecksum(archive, rel.SHA256) {
		if err := e.Fetcher.Verified(ctx, rel.URL, rel.SHA256, archive); err != nil {
			return err
		}
	}

	// Unpack beside the final directory and rename, so a crash never leaves a half-unpacked
	// runner where Check would look.
	staging := e.Layout.RunnerDir() + ".new"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := download.ExtractTarGz(archive, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	mark, _ := json.Marshal(runnerMark{Version: rel.Version, SHA256: rel.SHA256})
	if err := os.WriteFile(filepath.Join(staging, runnerMarker), mark, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(e.Layout.RunnerDir()); err != nil { // a partial earlier install, ours to replace
		return err
	}
	if err := os.Rename(staging, e.Layout.RunnerDir()); err != nil {
		return err
	}
	return e.State.Update(func(st *core.State) {
		st.RunnerName = e.Cfg.Runner.Name
		st.Target = e.Cfg.Runner.Target()
		st.RunnerVersion = rel.Version
		st.RunnerSHA256 = rel.SHA256
	})
}

// ---- work directory ----------------------------------------------------------------

type workDir struct{ e *Env }

func (*workDir) ID() string       { return "work-dir" }
func (*workDir) Describe() string { return "create the job work directory" }

func (s *workDir) Check(context.Context) (core.Status, error) {
	if _, err := os.Stat(filepath.Join(s.e.WorkDir(), workDirMarker)); err == nil {
		return core.Status{Done: true, Reason: s.e.WorkDir() + " ready"}, nil
	}
	return core.Status{Reason: s.e.WorkDir() + " missing"}, nil
}

func (s *workDir) Apply(context.Context) error {
	dir := filepath.Clean(s.e.WorkDir())
	if !filepath.IsAbs(dir) || dir == string(filepath.Separator) || dir == filepath.Clean(s.e.UserHome) {
		return diag.New(diag.CodeConfigInvalid, fmt.Sprintf("runner.work_dir %q is not a safe work directory", s.e.Cfg.Runner.WorkDir),
			"it is relative, the filesystem root or your whole home directory: remove would delete it",
			"set runner.work_dir to a dedicated directory such as ~/.bladerunner/work/"+s.e.Cfg.Runner.Name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return diag.Wrap(err, diag.CodeRegistrationFailed, "cannot create the work directory "+dir, "the path is not writable", "choose another runner.work_dir")
	}
	return os.WriteFile(filepath.Join(dir, workDirMarker), []byte(s.e.Cfg.Runner.Name+"\n"), 0o644)
}

// ---- registration ------------------------------------------------------------------

type registerRunner struct{ e *Env }

func (*registerRunner) ID() string { return "runner-registration" }
func (*registerRunner) Describe() string {
	return "register the runner with GitHub (short-lived token, never stored)"
}

// registration reads the .runner file config.sh writes. The runner writes it with a UTF-8
// byte-order mark (assumption A3, to be confirmed in BR-0).
func (s *registerRunner) registered() (name, url string, ok bool) {
	data, err := os.ReadFile(filepath.Join(s.e.Layout.RunnerDir(), ".runner"))
	if err != nil {
		return "", "", false
	}
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	var r struct {
		AgentName string `json:"agentName"`
		GitHubURL string `json:"gitHubUrl"`
	}
	if json.Unmarshal(data, &r) != nil || r.AgentName == "" {
		return "", "", false
	}
	return r.AgentName, strings.TrimRight(r.GitHubURL, "/"), true
}

func (s *registerRunner) Check(ctx context.Context) (core.Status, error) {
	if _, err := os.Stat(filepath.Join(s.e.Layout.RunnerDir(), "config.sh")); err != nil {
		return core.Status{Reason: "runner binary not installed yet"}, nil
	}
	name, url, ok := s.registered()
	want := strings.TrimRight(s.e.Provider.RegistrationURL(s.e.Scope()), "/")
	switch {
	case !ok:
		return core.Status{Reason: "not registered"}, nil
	case name != s.e.Cfg.Runner.Name || url != want:
		return core.Status{Reason: fmt.Sprintf("registered as %s at %s, config wants %s at %s", name, url, s.e.Cfg.Runner.Name, want)}, nil
	}
	// The local file cannot show what GitHub holds: the runner may have been deleted there,
	// or labels edited in the config. Ask GitHub, so apply converges to the config.
	runners, err := s.e.Provider.ListRunners(ctx, s.e.Scope())
	if err != nil {
		return core.Status{}, err
	}
	for _, r := range runners {
		if r.Name != name {
			continue
		}
		if missing := missingLabels(s.e.Cfg.Runner.AllLabels(s.e.GOOS, s.e.GOARCH), r.Labels); len(missing) > 0 {
			return core.Status{Reason: "GitHub lacks labels: " + strings.Join(missing, ", ")}, nil
		}
		return core.Status{Done: true, Reason: "registered as " + name}, nil
	}
	return core.Status{Reason: "GitHub has no runner named " + name}, nil
}

func missingLabels(want, have []string) []string {
	got := map[string]bool{}
	for _, l := range have {
		got[strings.ToLower(l)] = true
	}
	var missing []string
	for _, l := range want {
		if !got[strings.ToLower(l)] {
			missing = append(missing, l)
		}
	}
	return missing
}

func (s *registerRunner) Apply(ctx context.Context) error {
	e := s.e
	token, err := e.Provider.RegistrationToken(ctx, e.Scope())
	if err != nil {
		return err
	}
	args := []string{
		"--unattended", "--replace",
		"--url", e.Provider.RegistrationURL(e.Scope()),
		"--name", e.Cfg.Runner.Name,
		"--work", e.WorkDir(),
	}
	if extra := e.Cfg.Runner.ExtraLabels(e.GOOS, e.GOARCH); len(extra) > 0 {
		args = append(args, "--labels", strings.Join(extra, ","))
	}
	// The token goes in the environment, not argv: any local user can read argv from the
	// process table (spec section 10). The runner reads ACTIONS_RUNNER_INPUT_* (assumption A3).
	_, err = e.Exec.Run(ctx, execx.Cmd{
		Name: filepath.Join(e.Layout.RunnerDir(), "config.sh"),
		Args: args,
		Dir:  e.Layout.RunnerDir(),
		Env:  []string{"ACTIONS_RUNNER_INPUT_TOKEN=" + token},
	})
	if err != nil {
		return diag.Wrap(errors.New(redact(err.Error(), token)), diag.CodeRegistrationFailed,
			"the runner's configure script failed",
			"the registration token was refused or expired, the name or labels were rejected, or the runner's own dependencies are missing",
			"re-run `bladerunner apply` (a fresh token is minted each time); see the detail for the runner's message")
	}
	return nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// ---- service -----------------------------------------------------------------------

type serviceDefinition struct{ e *Env }

func (*serviceDefinition) ID() string { return "service-definition" }
func (*serviceDefinition) Describe() string {
	return "install the background service definition"
}

func (s *serviceDefinition) Check(ctx context.Context) (core.Status, error) {
	st, err := s.e.Platform.Status(ctx, s.e.Spec())
	if err != nil {
		return core.Status{}, err
	}
	switch {
	case !st.Installed:
		return core.Status{Reason: "service not installed"}, nil
	case !st.Current:
		return core.Status{Reason: "service definition is out of date"}, nil
	}
	return core.Status{Done: true, Reason: "service definition current"}, nil
}

func (s *serviceDefinition) Apply(ctx context.Context) error {
	return s.e.Platform.Install(ctx, s.e.Spec())
}

type serviceRunning struct{ e *Env }

func (*serviceRunning) ID() string       { return "service-running" }
func (*serviceRunning) Describe() string { return "start the runner service" }

func (s *serviceRunning) Check(ctx context.Context) (core.Status, error) {
	st, err := s.e.Platform.Status(ctx, s.e.Spec())
	if err != nil {
		return core.Status{}, err
	}
	if st.Running {
		return core.Status{Done: true, Reason: "service running"}, nil
	}
	return core.Status{Reason: "service not running (" + st.Detail + ")"}, nil
}

func (s *serviceRunning) Apply(ctx context.Context) error {
	return s.e.Platform.Start(ctx, s.e.Spec())
}
