package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

var macDefaults = Defaults{Hostname: "mac-mini-1", GOOS: "darwin"}

const specExample = `version: 1
bladerunner:
  min_version: "0.1.0"
runner:
  scope: repo
  repository: OWNER/REPO
  name: mac-mini-1
  labels: [self-hosted, macOS, ARM64]
  work_dir: ~/.bladerunner/work
  token:
    source: keychain
placement:
  default: auto
  github_labels: [ubuntu-latest]
  jobs:
    unit-tests: local
    e2e:        github
    deploy:     github
    lint:       auto
workflows:
  - .github/workflows/ci.yml
agent:
  listen: 127.0.0.1:7878
  poll_active_seconds: 10
  poll_idle_seconds: 60
`

func TestParseSpecExample(t *testing.T) {
	c, err := Parse([]byte(specExample), macDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runner.Repository != "OWNER/REPO" || c.Runner.Token.Source != TokenKeychain || c.Runner.WorkDir != "~/.bladerunner/work" {
		t.Errorf("runner = %+v", c.Runner)
	}
	if want := []string{"unit-tests", "e2e", "deploy", "lint"}; !reflect.DeepEqual(c.Placement.JobOrder, want) {
		t.Errorf("job order = %v, want %v", c.Placement.JobOrder, want)
	}
	for job, want := range map[string]Placement{"unit-tests": PlaceLocal, "e2e": PlaceGitHub, "lint": PlaceAuto, "unlisted": PlaceAuto} {
		if got := c.Placement.PlacementFor(job); got != want {
			t.Errorf("PlacementFor(%q) = %q, want %q", job, got, want)
		}
	}
	if !c.Placement.HasLocalOnlyJobs() {
		t.Error("HasLocalOnlyJobs = false, but unit-tests is local")
	}
}

func TestDefaults(t *testing.T) {
	minimal := "version: 1\nrunner:\n  scope: org\n  organization: acme\n"
	tests := []struct {
		name     string
		d        Defaults
		wantSrc  string
		wantName string
	}{
		{"mac", Defaults{Hostname: "mini", GOOS: "darwin"}, TokenKeychain, "mini"},
		{"linux", Defaults{Hostname: "box", GOOS: "linux"}, TokenFile, "box"},
		{"no hostname", Defaults{GOOS: "linux"}, TokenFile, "runner"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(minimal), tc.d)
			if err != nil {
				t.Fatal(err)
			}
			if c.Runner.Token.Source != tc.wantSrc || c.Runner.Name != tc.wantName {
				t.Errorf("source=%q name=%q", c.Runner.Token.Source, c.Runner.Name)
			}
			if want := "~/.bladerunner/work/" + tc.wantName; c.Runner.WorkDir != want {
				t.Errorf("work_dir = %q, want %q", c.Runner.WorkDir, want)
			}
			if c.Placement.Default != PlaceAuto || !reflect.DeepEqual(c.Placement.GitHubLabels, []string{"ubuntu-latest"}) {
				t.Errorf("placement = %+v", c.Placement)
			}
			if c.Agent.Listen != "127.0.0.1:7878" || c.Agent.PollActiveSeconds != 10 || c.Agent.PollIdleSeconds != 60 {
				t.Errorf("agent = %+v", c.Agent)
			}
		})
	}
}

func TestProblemsNameTheirKeyPath(t *testing.T) {
	base := "version: 1\nrunner:\n  scope: repo\n  repository: o/r\n"
	tests := []struct {
		name string
		src  string
		want []string // substrings that must each appear
	}{
		{"unknown top-level key", base + "runnr: x\n", []string{"runnr: unknown key"}},
		{"unknown nested key with hint", base + "  tokn:\n    source: file\n", []string{`runner.tokn: unknown key (did you mean "token"?)`}},
		{"unknown deep key", base + "  token:\n    sorce: file\n", []string{`runner.token.sorce: unknown key (did you mean "source"?)`}},
		{"bad version", "version: 2\nrunner:\n  scope: org\n  organization: a\n", []string{"version: must be 1"}},
		{"version not a number", "version: one\nrunner:\n  scope: org\n  organization: a\n", []string{"version: expected a whole number"}},
		{"missing scope", "version: 1\n", []string{"runner.scope: required"}},
		{"bad scope", "version: 1\nrunner:\n  scope: team\n", []string{"runner.scope: must be repo or org"}},
		{"repo scope needs repository", "version: 1\nrunner:\n  scope: repo\n", []string{"runner.repository: required when runner.scope is repo"}},
		{"repository shape", "version: 1\nrunner:\n  scope: repo\n  repository: justname\n", []string{"runner.repository"}},
		{"org with repository", "version: 1\nrunner:\n  scope: org\n  organization: a\n  repository: a/b\n", []string{"runner.repository: not allowed when runner.scope is org"}},
		{"repo with organization", base + "  organization: a\n", []string{"runner.organization: not allowed"}},
		{"bad name", base + "  name: \"has space\"\n", []string{"runner.name"}},
		{"label with comma", base + "  labels: [\"a,b\"]\n", []string{"runner.labels[0]"}},
		{"bad token source", base + "  token:\n    source: vault\n", []string{"runner.token.source: must be keychain, env or file"}},
		{"bad placement default", base + "placement:\n  default: sometimes\n", []string{"placement.default"}},
		{"bad job placement", base + "placement:\n  jobs:\n    e2e: cloud\n", []string{"placement.jobs.e2e"}},
		{"workflow escapes project", base + "workflows:\n  - ../other.yml\n", []string{"workflows[0]"}},
		{"absolute workflow", base + "workflows:\n  - /etc/x.yml\n", []string{"workflows[0]"}},
		{"bad min_version", base + "bladerunner:\n  min_version: soon\n", []string{"bladerunner.min_version"}},
		{"poll order", base + "agent:\n  poll_active_seconds: 30\n  poll_idle_seconds: 10\n", []string{"agent.poll_idle_seconds"}},
		{"poll zero", base + "agent:\n  poll_active_seconds: -1\n", []string{"agent.poll_active_seconds"}},
		{"jobs not a mapping", base + "placement:\n  jobs: [a]\n", []string{"placement.jobs: expected a mapping"}},
		{"syntax error", "version: 1\n\trunner: x\n", []string{"line 2", "tabs"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src), macDefaults)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := diag.CodeOf(err); got != diag.CodeConfigInvalid {
				t.Errorf("code = %q, want %q", got, diag.CodeConfigInvalid)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%s", w, err)
				}
			}
		})
	}
}

func TestAllProblemsReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte("version: 3\nrunner:\n  scope: repo\n  nmae: x\nagent:\n  listen: 0.0.0.0:80\n"), macDefaults)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The unknown key is found while decoding, so validation has not run yet: decoding
	// problems come first, and fixing them reveals the next layer.
	if !strings.Contains(err.Error(), "runner.nmae") {
		t.Errorf("missing decode problem:\n%s", err)
	}
	_, err = Parse([]byte("version: 3\nrunner:\n  scope: repo\nagent:\n  listen: 0.0.0.0:80\n"), macDefaults)
	for _, w := range []string{"version: must be 1", "runner.repository: required", "agent.listen"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("validation errors should be reported together; missing %q in:\n%s", w, err)
		}
	}
}

func TestListenMustBeLoopback(t *testing.T) {
	base := "version: 1\nrunner:\n  scope: org\n  organization: a\nagent:\n  listen: "
	for _, listen := range []string{"127.0.0.1:7878", "\"[::1]:7878\"", "localhost:9000", "127.0.0.2:1"} {
		if _, err := Parse([]byte(base+listen+"\n"), macDefaults); err != nil {
			t.Errorf("listen %q should be accepted: %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:7878", ":7878", "192.168.1.5:7878", "[::]:7878", "example.com:80", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:http"} {
		_, err := Parse([]byte(base+"\""+listen+"\"\n"), macDefaults)
		if err == nil || !strings.Contains(err.Error(), "agent.listen") {
			t.Errorf("listen %q must be rejected with an agent.listen error, got %v", listen, err)
		}
	}
}

func TestLabels(t *testing.T) {
	tests := []struct {
		name         string
		labels       []string
		goos, goarch string
		all, extra   []string
	}{
		{"implicit only", nil, "darwin", "arm64", []string{"self-hosted", "macOS", "ARM64"}, nil},
		{"user repeats implicit, any case", []string{"MACOS", "arm64", "gpu"}, "darwin", "arm64", []string{"self-hosted", "macOS", "ARM64", "gpu"}, []string{"gpu"}},
		{"implicit always first", []string{"gpu", "self-hosted"}, "linux", "amd64", []string{"self-hosted", "Linux", "X64", "gpu"}, []string{"gpu"}},
		{"duplicates collapse", []string{"a", "A", "a"}, "linux", "arm64", []string{"self-hosted", "Linux", "ARM64", "a"}, []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Runner{Labels: tc.labels}
			if got := r.AllLabels(tc.goos, tc.goarch); !reflect.DeepEqual(got, tc.all) {
				t.Errorf("AllLabels = %v, want %v", got, tc.all)
			}
			if got := r.ExtraLabels(tc.goos, tc.goarch); !reflect.DeepEqual(got, tc.extra) {
				t.Errorf("ExtraLabels = %v, want %v", got, tc.extra)
			}
		})
	}
}

func TestHasLocalOnlyJobs(t *testing.T) {
	tests := []struct {
		p    PlacementConfig
		want bool
	}{
		{PlacementConfig{Default: PlaceAuto}, false},
		{PlacementConfig{Default: PlaceGitHub}, false},
		{PlacementConfig{Default: PlaceLocal}, true},
		{PlacementConfig{Default: PlaceAuto, Jobs: map[string]Placement{"a": PlaceLocal}}, true},
		{PlacementConfig{Default: PlaceAuto, Jobs: map[string]Placement{"a": PlaceGitHub}}, false},
	}
	for i, tc := range tests {
		if got := tc.p.HasLocalOnlyJobs(); got != tc.want {
			t.Errorf("case %d: got %v, want %v", i, got, tc.want)
		}
	}
}

func TestRenderRoundTrips(t *testing.T) {
	orig, err := Parse([]byte(specExample), macDefaults)
	if err != nil {
		t.Fatal(err)
	}
	out := Render(orig)
	if !strings.HasPrefix(string(out), "# bladerunner.yaml") {
		t.Errorf("missing header:\n%s", out)
	}
	back, err := Parse(out, macDefaults)
	if err != nil {
		t.Fatalf("rendered config does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Errorf("round trip changed the config\nbefore: %+v\nafter:  %+v\n%s", orig, back, out)
	}
	if strings.Contains(string(out), "ghp_") || strings.Contains(strings.ToLower(string(out)), "password") {
		t.Error("rendered config must not contain anything token-like")
	}
}

func TestLoadMissingAndPresent(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "bladerunner.yaml"), macDefaults)
	if diag.CodeOf(err) != diag.CodeConfigMissing {
		t.Errorf("missing file: code = %q, err = %v", diag.CodeOf(err), err)
	}
	var de *diag.Error
	if !errors.As(err, &de) || !strings.Contains(de.Fix, "bladerunner init") {
		t.Errorf("fix should point at `bladerunner init`: %v", err)
	}

	path := filepath.Join(dir, "bladerunner.yaml")
	if err := os.WriteFile(path, []byte(specExample), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, macDefaults)
	if err != nil || c.Runner.Name != "mac-mini-1" {
		t.Errorf("Load = %+v, %v", c, err)
	}
}
