package config

import (
	"fmt"
	"strings"

	"github.com/modullar/blade-runner/internal/yamlsubset"
)

// Header tops the file `init` writes. Unlike the generated workflows, this file is the
// source of truth and meant to be edited, so its header says so.
const Header = "# bladerunner.yaml - created by `bladerunner init`.\n" +
	"# This file is the source of truth for the runner and for job placement. Edit it, then\n" +
	"# run `bladerunner apply` (machine) and `bladerunner generate` (workflows).\n" +
	"# It contains no secrets: the token lives in the keychain, a 0600 file, or the environment.\n"

// Render writes c as bladerunner.yaml text that Parse reads back to an equal Config.
func Render(c *Config) []byte {
	var b strings.Builder
	q := yamlsubset.Quote
	list := func(items []string) string {
		qs := make([]string, len(items))
		for i, s := range items {
			qs[i] = q(s)
		}
		return "[" + strings.Join(qs, ", ") + "]"
	}

	b.WriteString(Header)
	fmt.Fprintf(&b, "version: %d\n", c.Version)
	if c.BladeRunner.MinVersion != "" {
		fmt.Fprintf(&b, "bladerunner:\n  min_version: %s\n", q(c.BladeRunner.MinVersion))
	}
	b.WriteString("runner:\n")
	fmt.Fprintf(&b, "  scope: %s\n", q(c.Runner.Scope))
	if c.Runner.Repository != "" {
		fmt.Fprintf(&b, "  repository: %s\n", q(c.Runner.Repository))
	}
	if c.Runner.Organization != "" {
		fmt.Fprintf(&b, "  organization: %s\n", q(c.Runner.Organization))
	}
	fmt.Fprintf(&b, "  name: %s\n", q(c.Runner.Name))
	if len(c.Runner.Labels) > 0 {
		fmt.Fprintf(&b, "  labels: %s\n", list(c.Runner.Labels))
	}
	fmt.Fprintf(&b, "  work_dir: %s\n", q(c.Runner.WorkDir))
	fmt.Fprintf(&b, "  token:\n    source: %s\n", q(c.Runner.Token.Source))

	b.WriteString("placement:\n")
	fmt.Fprintf(&b, "  default: %s\n", q(string(c.Placement.Default)))
	fmt.Fprintf(&b, "  github_labels: %s\n", list(c.Placement.GitHubLabels))
	if len(c.Placement.JobOrder) > 0 {
		b.WriteString("  jobs:\n")
		for _, job := range c.Placement.JobOrder {
			fmt.Fprintf(&b, "    %s: %s\n", q(job), q(string(c.Placement.Jobs[job])))
		}
	}
	if len(c.Workflows) > 0 {
		b.WriteString("workflows:\n")
		for _, w := range c.Workflows {
			fmt.Fprintf(&b, "  - %s\n", q(w))
		}
	}
	fmt.Fprintf(&b, "agent:\n  listen: %s\n  poll_active_seconds: %d\n  poll_idle_seconds: %d\n",
		q(c.Agent.Listen), c.Agent.PollActiveSeconds, c.Agent.PollIdleSeconds)
	return []byte(b.String())
}
