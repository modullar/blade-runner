package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/yamlsubset"
)

// Problem is one thing wrong with the file, tied to the key path that caused it.
type Problem struct {
	Path string
	Msg  string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Msg
	}
	return p.Path + ": " + p.Msg
}

// Load reads and validates the file at path.
func Load(path string, d Defaults) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, diag.Wrap(err, diag.CodeConfigMissing,
				fmt.Sprintf("cannot find %s", path),
				"this project has no Blade Runner config yet, or you are in another directory",
				"run `bladerunner init` here, or pass --config <path>")
		}
		return nil, diag.Wrap(err, diag.CodeConfigMissing, fmt.Sprintf("cannot read %s", path),
			"the file is unreadable", "check its permissions")
	}
	return Parse(data, d)
}

// Parse decodes, defaults and validates config text. Every problem found is reported at
// once, each naming its key path.
func Parse(data []byte, d Defaults) (*Config, error) {
	root, err := yamlsubset.Parse(data)
	if err != nil {
		return nil, invalid([]Problem{{Msg: err.Error()}})
	}
	c, problems := decode(root)
	if len(problems) > 0 {
		return nil, invalid(problems)
	}
	applyDefaults(c, d)
	if problems := Validate(c); len(problems) > 0 {
		return nil, invalid(problems)
	}
	return c, nil
}

// Finalize defaults and validates a Config built in code (by `init`), with the same rules
// and the same key-path errors as a file.
func Finalize(c *Config, d Defaults) error {
	applyDefaults(c, d)
	if problems := Validate(c); len(problems) > 0 {
		return invalid(problems)
	}
	return nil
}

func invalid(problems []Problem) error {
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = p.String()
	}
	return diag.New(diag.CodeConfigInvalid, "bladerunner.yaml is invalid",
		"\n    "+strings.Join(lines, "\n    "),
		"edit the keys named above (unknown keys are errors so typos cannot pass silently), then re-run")
}

type decoder struct {
	problems []Problem
}

func (d *decoder) add(path, format string, args ...any) {
	d.problems = append(d.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

type fieldFn func(n *yamlsubset.Node, path string)

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// mapping walks n's keys against the known fields; an unknown key is a problem.
func (d *decoder) mapping(n *yamlsubset.Node, path string, fields map[string]fieldFn) {
	if n == nil || n.Kind == yamlsubset.Null {
		return
	}
	if n.Kind != yamlsubset.Map {
		d.add(path, "expected a mapping of keys")
		return
	}
	for _, k := range n.Keys {
		fn, ok := fields[k]
		if !ok {
			msg := "unknown key"
			if s := closest(k, fields); s != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", s)
			}
			d.add(join(path, k), "%s", msg)
			continue
		}
		fn(n.Map[k], join(path, k))
	}
}

func (d *decoder) str(n *yamlsubset.Node, path string) string {
	switch n.Kind {
	case yamlsubset.Null:
		return ""
	case yamlsubset.Scalar:
		return n.Value
	}
	d.add(path, "expected a single value")
	return ""
}

func (d *decoder) integer(n *yamlsubset.Node, path string) int {
	if n.Kind == yamlsubset.Null {
		return 0
	}
	if n.Kind != yamlsubset.Scalar || n.Quoted {
		d.add(path, "expected a whole number")
		return 0
	}
	v, err := strconv.Atoi(n.Value)
	if err != nil {
		d.add(path, "expected a whole number, got %q", n.Value)
		return 0
	}
	return v
}

func (d *decoder) strList(n *yamlsubset.Node, path string) []string {
	if n.Kind == yamlsubset.Null {
		return nil
	}
	if n.Kind != yamlsubset.List {
		d.add(path, "expected a list")
		return nil
	}
	out := make([]string, 0, len(n.Items))
	for i, it := range n.Items {
		if it.Kind != yamlsubset.Scalar {
			d.add(fmt.Sprintf("%s[%d]", path, i), "expected a single value")
			continue
		}
		out = append(out, it.Value)
	}
	return out
}

func decode(root *yamlsubset.Node) (*Config, []Problem) {
	d := &decoder{}
	c := &Config{}
	if root.Kind != yamlsubset.Map {
		d.add("", "the file must be a mapping of keys")
		return nil, d.problems
	}
	d.mapping(root, "", map[string]fieldFn{
		"version": func(n *yamlsubset.Node, p string) { c.Version = d.integer(n, p) },
		"bladerunner": func(n *yamlsubset.Node, p string) {
			d.mapping(n, p, map[string]fieldFn{
				"min_version": func(n *yamlsubset.Node, p string) { c.BladeRunner.MinVersion = d.str(n, p) },
			})
		},
		"runner": func(n *yamlsubset.Node, p string) {
			d.mapping(n, p, map[string]fieldFn{
				"scope":          func(n *yamlsubset.Node, p string) { c.Runner.Scope = d.str(n, p) },
				"repository":     func(n *yamlsubset.Node, p string) { c.Runner.Repository = d.str(n, p) },
				"organization":   func(n *yamlsubset.Node, p string) { c.Runner.Organization = d.str(n, p) },
				"name":           func(n *yamlsubset.Node, p string) { c.Runner.Name = d.str(n, p) },
				"labels":         func(n *yamlsubset.Node, p string) { c.Runner.Labels = d.strList(n, p) },
				"work_dir":       func(n *yamlsubset.Node, p string) { c.Runner.WorkDir = d.str(n, p) },
				"trusted_actors": func(n *yamlsubset.Node, p string) { c.Runner.TrustedActors = d.strList(n, p) },
				"token": func(n *yamlsubset.Node, p string) {
					d.mapping(n, p, map[string]fieldFn{
						"source": func(n *yamlsubset.Node, p string) { c.Runner.Token.Source = d.str(n, p) },
					})
				},
			})
		},
		"placement": func(n *yamlsubset.Node, p string) {
			d.mapping(n, p, map[string]fieldFn{
				"default":       func(n *yamlsubset.Node, p string) { c.Placement.Default = Placement(d.str(n, p)) },
				"github_labels": func(n *yamlsubset.Node, p string) { c.Placement.GitHubLabels = d.strList(n, p) },
				"jobs": func(n *yamlsubset.Node, p string) {
					if n.Kind == yamlsubset.Null {
						return
					}
					if n.Kind != yamlsubset.Map {
						d.add(p, "expected a mapping of job name to placement")
						return
					}
					c.Placement.Jobs = map[string]Placement{}
					for _, k := range n.Keys {
						c.Placement.JobOrder = append(c.Placement.JobOrder, k)
						c.Placement.Jobs[k] = Placement(d.str(n.Map[k], join(p, k)))
					}
				},
			})
		},
		"workflows": func(n *yamlsubset.Node, p string) { c.Workflows = d.strList(n, p) },
		"agent": func(n *yamlsubset.Node, p string) {
			d.mapping(n, p, map[string]fieldFn{
				"listen":              func(n *yamlsubset.Node, p string) { c.Agent.Listen = d.str(n, p) },
				"poll_active_seconds": func(n *yamlsubset.Node, p string) { c.Agent.PollActiveSeconds = d.integer(n, p) },
				"poll_idle_seconds":   func(n *yamlsubset.Node, p string) { c.Agent.PollIdleSeconds = d.integer(n, p) },
			})
		},
	})
	return c, d.problems
}

// closest returns the field name within edit distance 2 of key, for typo hints.
func closest(key string, fields map[string]fieldFn) string {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	best, bestDist := "", 3
	for _, n := range names {
		if dist := editDistance(strings.ToLower(key), n); dist < bestDist {
			best, bestDist = n, dist
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// applyDefaults fills omitted keys. It never overrides what the file set.
func applyDefaults(c *Config, d Defaults) {
	if c.Runner.Name == "" {
		c.Runner.Name = d.Hostname
		if c.Runner.Name == "" {
			c.Runner.Name = "runner"
		}
	}
	if len(c.Runner.TrustedActors) == 0 && c.Runner.Scope == ScopeRepo {
		if owner, _, ok := strings.Cut(c.Runner.Repository, "/"); ok {
			c.Runner.TrustedActors = []string{owner}
		}
	}
	if c.Runner.WorkDir == "" {
		c.Runner.WorkDir = "~/.bladerunner/work/" + c.Runner.Name
	}
	if c.Runner.Token.Source == "" {
		if d.GOOS == "darwin" {
			c.Runner.Token.Source = TokenKeychain
		} else {
			c.Runner.Token.Source = TokenFile
		}
	}
	if c.Placement.Default == "" {
		c.Placement.Default = PlaceAuto
	}
	if len(c.Placement.GitHubLabels) == 0 {
		c.Placement.GitHubLabels = []string{"ubuntu-latest"}
	}
	if c.Agent.Listen == "" {
		c.Agent.Listen = "127.0.0.1:7878"
	}
	if c.Agent.PollActiveSeconds == 0 {
		c.Agent.PollActiveSeconds = 10
	}
	if c.Agent.PollIdleSeconds == 0 {
		c.Agent.PollIdleSeconds = 60
	}
}
