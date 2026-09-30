package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
)

// RemoveSteps undo apply, in an order that keeps what later steps need until they have run:
// stop the service first (GitHub will not deregister a runner that is busy), deregister
// while the token still exists, and delete local state last. Project files such as
// bladerunner.yaml and the workflows are never touched.
func RemoveSteps(e *Env) []core.Step {
	return []core.Step{
		&removeService{e}, &deregister{e}, &removeRunnerFiles{e}, &removeWorkDir{e}, &removeToken{e}, &removeState{e},
	}
}

type removeService struct{ e *Env }

func (*removeService) ID() string       { return "service-removed" }
func (*removeService) Describe() string { return "stop and uninstall the runner service" }

func (s *removeService) Check(ctx context.Context) (core.Status, error) {
	st, err := s.e.Platform.Status(ctx, s.e.Spec())
	if err != nil {
		return core.Status{}, err
	}
	if !st.Installed && !st.Running {
		return core.Status{Done: true, Reason: "no service installed"}, nil
	}
	return core.Status{Reason: "service installed"}, nil
}

func (s *removeService) Apply(ctx context.Context) error {
	return s.e.Platform.Uninstall(ctx, s.e.Spec())
}

type deregister struct{ e *Env }

func (*deregister) ID() string       { return "github-registration" }
func (*deregister) Describe() string { return "deregister the runner from GitHub" }

func (s *deregister) find(ctx context.Context) ([]int64, error) {
	runners, err := s.e.Provider.ListRunners(ctx, s.e.Scope())
	if err != nil {
		var de *diag.Error
		if errors.As(err, &de) && de.Code == diag.CodeTokenMissing {
			return nil, withSkipHint(de)
		}
		return nil, err
	}
	var ids []int64
	for _, r := range runners {
		if r.Name == s.e.Cfg.Runner.Name {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func withSkipHint(de *diag.Error) error {
	cp := *de
	cp.Fix += "; or pass --skip-deregister and delete the runner under the repository's Settings > Actions > Runners"
	return &cp
}

func (s *deregister) Check(ctx context.Context) (core.Status, error) {
	if s.e.SkipDeregister {
		return core.Status{Done: true, Reason: "skipped (--skip-deregister): delete the runner in GitHub's settings"}, nil
	}
	ids, err := s.find(ctx)
	if err != nil {
		// A repeated remove has no token any more and nothing local either: there is
		// nothing it could deregister, and failing would make remove non-idempotent.
		if _, statErr := os.Lstat(s.e.Layout.RunnerHome()); diag.CodeOf(err) == diag.CodeTokenMissing && statErr != nil {
			return core.Status{Done: true, Reason: "no local state and no token: if the runner still shows under Settings > Actions > Runners, delete it there"}, nil
		}
		return core.Status{}, err
	}
	if len(ids) == 0 {
		return core.Status{Done: true, Reason: "not registered with GitHub"}, nil
	}
	return core.Status{Reason: fmt.Sprintf("registered as %s", s.e.Cfg.Runner.Name)}, nil
}

func (s *deregister) Apply(ctx context.Context) error {
	ids, err := s.find(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.e.Provider.RemoveRunner(ctx, s.e.Scope(), id); err != nil {
			return err
		}
	}
	return nil
}

type removeRunnerFiles struct{ e *Env }

func (*removeRunnerFiles) ID() string       { return "runner-files" }
func (*removeRunnerFiles) Describe() string { return "delete the runner, its downloads and its logs" }

func (s *removeRunnerFiles) paths() []string {
	l := s.e.Layout
	return []string{l.RunnerDir(), l.RunnerDir() + ".new", l.DownloadDir(), l.LogDir()}
}

func (s *removeRunnerFiles) Check(context.Context) (core.Status, error) {
	for _, p := range s.paths() {
		if _, err := os.Lstat(p); err == nil {
			return core.Status{Reason: p + " exists"}, nil
		}
	}
	return core.Status{Done: true, Reason: "no runner files"}, nil
}

func (s *removeRunnerFiles) Apply(context.Context) error {
	for _, p := range s.paths() {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

type removeWorkDir struct{ e *Env }

func (*removeWorkDir) ID() string       { return "work-dir-removed" }
func (*removeWorkDir) Describe() string { return "delete the job work directory" }

// owned says whether Blade Runner may delete the work directory: only one it created for
// this runner (its marker file names the runner), and never the filesystem root or the
// user's home, however the config was edited.
func (s *removeWorkDir) owned() (ok bool, why string) {
	dir := filepath.Clean(s.e.WorkDir())
	if !filepath.IsAbs(dir) || dir == string(filepath.Separator) || dir == filepath.Clean(s.e.UserHome) {
		return false, "left in place: not a safe path to delete"
	}
	data, err := os.ReadFile(filepath.Join(dir, workDirMarker))
	if err != nil {
		return false, "left in place: Blade Runner did not create it"
	}
	if owner := strings.TrimSpace(string(data)); owner != s.e.Cfg.Runner.Name {
		return false, "left in place: it belongs to runner " + owner
	}
	return true, ""
}

func (s *removeWorkDir) Check(context.Context) (core.Status, error) {
	ok, why := s.owned()
	if !ok {
		return core.Status{Done: true, Reason: why}, nil
	}
	return core.Status{Reason: s.e.WorkDir() + " exists"}, nil
}

func (s *removeWorkDir) Apply(context.Context) error {
	if ok, _ := s.owned(); !ok {
		return nil
	}
	return os.RemoveAll(filepath.Clean(s.e.WorkDir()))
}

type removeToken struct{ e *Env }

func (*removeToken) ID() string       { return "token-removed" }
func (*removeToken) Describe() string { return "delete the stored GitHub token" }

func (s *removeToken) Check(ctx context.Context) (core.Status, error) {
	if s.e.Cfg.Runner.Token.Source == config.TokenEnv {
		return core.Status{Done: true, Reason: "token comes from the environment: nothing stored"}, nil
	}
	_, err := s.e.Secrets.Get(ctx, s.e.Cfg.Runner.Name)
	if diag.CodeOf(err) == diag.CodeTokenMissing {
		return core.Status{Done: true, Reason: "no stored token"}, nil
	}
	return core.Status{Reason: "a token is stored"}, nil
}

func (s *removeToken) Apply(ctx context.Context) error {
	return s.e.Secrets.Delete(ctx, s.e.Cfg.Runner.Name)
}

type removeState struct{ e *Env }

func (*removeState) ID() string       { return "local-state" }
func (*removeState) Describe() string { return "delete this runner's local state" }

func (s *removeState) Check(context.Context) (core.Status, error) {
	if _, err := os.Lstat(s.e.Layout.RunnerHome()); err != nil {
		return core.Status{Done: true, Reason: "no local state"}, nil
	}
	return core.Status{Reason: s.e.Layout.RunnerHome() + " exists"}, nil
}

func (s *removeState) Apply(context.Context) error {
	if err := os.RemoveAll(s.e.Layout.RunnerHome()); err != nil {
		return err
	}
	// Tidy the shared directories, but only if this was the last runner: os.Remove fails on
	// a non-empty directory, which is exactly the check we want.
	l := s.e.Layout
	for _, dir := range []string{
		filepath.Join(l.Home, "runners"), l.SecretsDir(), filepath.Join(l.Home, "work"), l.Home,
	} {
		_ = os.Remove(dir)
	}
	return nil
}
