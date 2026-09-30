// Package doctor diagnoses a Blade Runner installation. Every finding carries a stable error
// code, the fix, and (through diag) a docs link, so a red check is actionable on its own.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/version"
)

// Level is a finding's severity.
type Level int

const (
	Pass Level = iota
	Warn
	Fail
	Skip // not run because something it depends on failed
)

func (l Level) String() string { return [...]string{"ok", "warn", "FAIL", "skip"}[l] }

// Result is one check's finding.
type Result struct {
	ID      string
	Level   Level
	Code    string // set when Level is Warn or Fail
	Message string
	Fix     string
}

// Docs is the docs link for the finding's code, or "".
func (r Result) Docs() string {
	if r.Code == "" {
		return ""
	}
	return diag.DocsURL(r.Code)
}

// Options are the facts doctor compares against.
type Options struct {
	CLIVersion string
	// FreeBytes reports free disk space at path; defaults to the real filesystem.
	FreeBytes func(path string) (uint64, error)
}

// Disk thresholds (assumption A7-style defaults, to be tuned with real job data): a runner
// checkout plus caches fills a disk fast, and a full disk fails jobs in confusing ways.
const (
	warnFreeBytes = 10 << 30
	failFreeBytes = 2 << 30
)

// Run executes every check. It never changes anything and always returns a result per check.
func Run(ctx context.Context, e *install.Env, o Options) []Result {
	if o.FreeBytes == nil {
		o.FreeBytes = FreeBytes
	}
	d := &doctor{e: e, o: o}
	d.cliVersion()
	d.prereqs(ctx)
	d.token(ctx)
	d.tokenValid(ctx)
	d.publicRepo(ctx)
	d.registration(ctx)
	d.service(ctx)
	d.disk()
	d.localPlacement()
	return d.results
}

// Failed reports whether any result is a failure.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

type doctor struct {
	e       *install.Env
	o       Options
	results []Result

	tokenOK bool // a token was found
	authOK  bool // GitHub accepted it
}

func (d *doctor) add(r Result) { d.results = append(d.results, r) }

func (d *doctor) pass(id, msg string) { d.add(Result{ID: id, Level: Pass, Message: msg}) }
func (d *doctor) skip(id, why string) { d.add(Result{ID: id, Level: Skip, Message: why}) }
func (d *doctor) finding(id string, lvl Level, code, msg, fix string) {
	d.add(Result{ID: id, Level: lvl, Code: code, Message: msg, Fix: fix})
}

// fromErr turns a diag error into a finding, keeping its code, message and fix.
func (d *doctor) fromErr(id string, lvl Level, err error) {
	var de *diag.Error
	if errors.As(err, &de) {
		d.finding(id, lvl, de.Code, de.What+" ("+de.Cause+")", de.Fix)
		return
	}
	d.finding(id, lvl, diag.CodeGitHubUnavailable, err.Error(), "re-run `bladerunner doctor`")
}

func (d *doctor) cliVersion() {
	const id = "cli-version"
	min := d.e.Cfg.BladeRunner.MinVersion
	if min == "" {
		d.pass(id, "no minimum version pinned (bladerunner.min_version)")
		return
	}
	older, err := version.Less(d.o.CLIVersion, min)
	switch {
	case err != nil:
		d.finding(id, Warn, diag.CodeCLIOutdated, fmt.Sprintf("cannot compare CLI version %q with min_version %q: %v", d.o.CLIVersion, min, err), "fix bladerunner.min_version")
	case older:
		d.finding(id, Warn, diag.CodeCLIOutdated,
			fmt.Sprintf("this bladerunner is %s but the project requires at least %s", d.o.CLIVersion, min),
			"upgrade bladerunner to "+min+" or newer")
	default:
		d.pass(id, fmt.Sprintf("bladerunner %s satisfies min_version %s", d.o.CLIVersion, min))
	}
}

func (d *doctor) prereqs(ctx context.Context) {
	const id = "prerequisites"
	var bad []string
	var fixes []string
	for _, p := range d.e.Platform.CheckPrereqs(ctx) {
		if !p.OK {
			bad = append(bad, p.Name)
			fixes = append(fixes, p.Name+": "+p.Fix)
		}
	}
	if len(bad) > 0 {
		d.finding(id, Fail, diag.CodePrereqMissing, "missing prerequisites: "+strings.Join(bad, ", "), strings.Join(fixes, "; "))
		return
	}
	d.pass(id, "all prerequisites present")
}

func (d *doctor) token(ctx context.Context) {
	const id = "token"
	if _, err := d.e.Token(ctx); err != nil {
		d.fromErr(id, Fail, err)
		return
	}
	d.tokenOK = true
	d.pass(id, "a token is stored ("+d.e.Secrets.Source()+")")
}

func (d *doctor) tokenValid(ctx context.Context) {
	const id = "token-valid"
	if !d.tokenOK {
		d.skip(id, "no token to check")
		return
	}
	if err := d.e.Provider.CheckAuth(ctx, d.e.Scope()); err != nil {
		d.fromErr(id, Fail, err)
		return
	}
	d.authOK = true
	d.pass(id, "GitHub accepts the token and it can manage runners for "+d.e.Cfg.Runner.Target())
}

func (d *doctor) publicRepo(ctx context.Context) {
	const id = "public-repo"
	if d.e.Cfg.Runner.Scope != "repo" {
		d.pass(id, "organization scope: check the runner group excludes public repositories")
		return
	}
	if !d.authOK {
		d.skip(id, "cannot reach GitHub with a valid token")
		return
	}
	vis, err := d.e.Provider.Visibility(ctx, d.e.Cfg.Runner.Repository)
	switch {
	case err != nil:
		d.fromErr(id, Fail, err)
	case vis == provider.Public:
		d.finding(id, Fail, diag.CodePublicRepoRefused,
			d.e.Cfg.Runner.Repository+" is public: fork pull requests could run code on this machine",
			"use GitHub-hosted runners for it (placement: github) and run `bladerunner remove`")
	default:
		d.pass(id, d.e.Cfg.Runner.Repository+" is private")
	}
}

func (d *doctor) registration(ctx context.Context) {
	const id = "runner-registered"
	if !d.authOK {
		d.skip(id, "cannot reach GitHub with a valid token")
		return
	}
	runners, err := d.e.Provider.ListRunners(ctx, d.e.Scope())
	if err != nil {
		d.fromErr(id, Fail, err)
		return
	}
	name := d.e.Cfg.Runner.Name
	for _, r := range runners {
		if r.Name != name {
			continue
		}
		if !r.Online() {
			d.finding(id, Fail, diag.CodeRunnerOffline, "GitHub sees runner "+name+" as offline",
				"check the service (see the next check), then `bladerunner logs`")
			return
		}
		want := d.e.Cfg.Runner.AllLabels(d.e.GOOS, d.e.GOARCH)
		if missing := missingLabels(want, r.Labels); len(missing) > 0 {
			d.finding(id, Fail, diag.CodeRunnerNotRegistered, "runner "+name+" lacks labels "+strings.Join(missing, ", ")+" on GitHub",
				"run `bladerunner apply` to re-register with the configured labels")
			return
		}
		d.pass(id, "runner "+name+" is online on GitHub")
		return
	}
	d.finding(id, Fail, diag.CodeRunnerNotRegistered, "GitHub has no runner named "+name+" for "+d.e.Cfg.Runner.Target(),
		"run `bladerunner apply`")
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

func (d *doctor) service(ctx context.Context) {
	const id = "service"
	st, err := d.e.Platform.Status(ctx, d.e.Spec())
	switch {
	case err != nil:
		d.fromErr(id, Fail, err)
	case !st.Installed:
		d.finding(id, Fail, diag.CodeServiceInstall, "the runner service is not installed", "run `bladerunner apply`")
	case !st.Running:
		d.finding(id, Fail, diag.CodeServiceNotRunning, "the runner service is installed but not running ("+st.Detail+")",
			"run `bladerunner apply` to start it, then `bladerunner logs` if it stops again")
	case !st.Current:
		d.finding(id, Warn, diag.CodeServiceInstall, "the service definition differs from what the config would generate",
			"run `bladerunner apply` to bring it up to date")
	default:
		d.pass(id, "service installed and running ("+st.Detail+")")
	}
}

func (d *doctor) disk() {
	const id = "disk-space"
	path := nearestExisting(d.e.WorkDir())
	free, err := d.o.FreeBytes(path)
	if err != nil {
		d.finding(id, Warn, diag.CodeDiskLow, "cannot read free disk space at "+path+": "+err.Error(), "check the filesystem")
		return
	}
	gib := func(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
	switch {
	case free < failFreeBytes:
		d.finding(id, Fail, diag.CodeDiskLow, fmt.Sprintf("only %s free at %s", gib(free), path), "free disk space: jobs fail when the work directory fills up")
	case free < warnFreeBytes:
		d.finding(id, Warn, diag.CodeDiskLow, fmt.Sprintf("%s free at %s", gib(free), path), "free some disk space soon: checkouts and caches grow")
	default:
		d.pass(id, fmt.Sprintf("%s free at %s", gib(free), path))
	}
}

// nearestExisting walks up to the first directory that exists, so the check works before
// the work directory has been created.
func nearestExisting(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func (d *doctor) localPlacement() {
	const id = "local-placement"
	if d.e.Cfg.Placement.HasLocalOnlyJobs() {
		d.finding(id, Warn, diag.CodeLocalPlacement,
			"some jobs are pinned to the local runner (placement: local): they queue, and wait, while the runner is down",
			"use placement: auto for jobs that may fall back to GitHub-hosted runners")
		return
	}
	d.pass(id, "no job is pinned to the local runner")
}
