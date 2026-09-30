// Package guard checks the project's workflow files: every job that can land on the local
// runner must be locked to a trusted actor, and workflows must not use triggers outsiders fire.
//
// What this is, and is not. It is DEFENCE IN DEPTH: it catches honest mistakes, keeps
// unauthorized jobs from ever being queued for the runner, and makes the intent reviewable.
// It is NOT the enforcement. A workflow file is written by whoever opens the pull request or
// pushes the branch, so an attacker simply deletes the guard in their own copy. The
// enforcement that an attacker cannot edit is the job hook on the machine (internal/hook).
//
// The reader is a line-oriented scan, not a YAML parser, and it fails closed: anything it
// cannot classify (a flow-style `on`, an unrecognized trigger) is reported, not assumed safe.
package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Policy is what the scan checks workflows against.
type Policy struct {
	TrustedActors []string
	// LocalLabels are the runner's labels (self-hosted, OS, arch and extras). A job whose
	// runs-on mentions one of them, or is not a plain GitHub-hosted image, may run locally.
	LocalLabels []string
}

// Finding is one violation.
type Finding struct {
	File    string
	Job     string // empty for a file-level finding
	Message string
	Fix     string
}

func (f Finding) String() string {
	where := f.File
	if f.Job != "" {
		where += ", job " + f.Job
	}
	s := where + ": " + f.Message
	if f.Fix != "" {
		s += " (fix: " + f.Fix + ")"
	}
	return s
}

// Report is the result of scanning a project.
type Report struct {
	Files      int
	Jobs       int
	LocalJobs  int // jobs that may run on the local runner
	Findings   []Finding
	Unreadable []string
}

// allowedTriggers are events only people with write access to this repository can cause
// (or that carry no outside input). pull_request is allowed but needs the same-repo clause.
var allowedTriggers = map[string]bool{
	"push": true, "pull_request": true, "workflow_dispatch": true, "schedule": true, "merge_group": true,
}

var hostedRe = regexp.MustCompile(`^(ubuntu|windows|macos)-[0-9a-z][0-9a-z.\-]*$`)

// ScanDir scans every workflow file under <project>/.github/workflows.
func ScanDir(project string, p Policy) (Report, error) {
	dir := filepath.Join(project, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return Report{}, nil
	}
	if err != nil {
		return Report{}, err
	}
	var rep Report
	var names []string
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); !e.IsDir() && (ext == ".yml" || ext == ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			rep.Unreadable = append(rep.Unreadable, name)
			rep.Findings = append(rep.Findings, Finding{File: name, Message: "cannot be read, so it cannot be checked: " + err.Error()})
			continue
		}
		rep.Files++
		jobs, local, findings := ScanFile(name, src, p)
		rep.Jobs += jobs
		rep.LocalJobs += local
		rep.Findings = append(rep.Findings, findings...)
	}
	return rep, nil
}

type line struct {
	indent int
	text   string
}

func readLines(src []byte) []line {
	var out []line
	for _, raw := range strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n") {
		t := strings.TrimRight(stripComment(raw), " \t")
		if strings.TrimSpace(t) == "" {
			continue
		}
		ind := len(t) - len(strings.TrimLeft(t, " "))
		out = append(out, line{indent: ind, text: strings.TrimSpace(t)})
	}
	return out
}

func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case (c == '\'' || c == '"') && (i == 0 || strings.ContainsRune(" \t:[,", rune(s[i-1]))):
			q = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// key splits "name: rest"; ok is false for lines that are not a key.
func key(text string) (name, rest string, ok bool) {
	i := strings.Index(text, ":")
	for i >= 0 && i+1 < len(text) && text[i+1] != ' ' {
		j := strings.Index(text[i+1:], ":")
		if j < 0 {
			i = -1
			break
		}
		i += 1 + j
	}
	if i < 0 {
		return "", "", false
	}
	name = strings.Trim(strings.TrimSpace(text[:i]), `"'`)
	return name, strings.TrimSpace(text[i+1:]), true
}

// section returns the header's inline value and the indented lines that follow it.
func section(lines []line, start int) (rest string, body []line, next int) {
	_, rest, _ = key(lines[start].text)
	i := start + 1
	for i < len(lines) && lines[i].indent > lines[start].indent {
		body = append(body, lines[i])
		i++
	}
	return rest, body, i
}

// ScanFile checks one workflow and returns (jobs, jobs that may run locally, findings).
func ScanFile(name string, src []byte, p Policy) (int, int, []Finding) {
	lines := readLines(src)
	var triggers []string
	triggersKnown := false
	type job struct {
		name, runsOn, ifExpr string
		hasUses              bool
	}
	var jobs []job

	for i := 0; i < len(lines); {
		l := lines[i]
		k, _, isKey := key(l.text)
		if l.indent != 0 || !isKey {
			i++
			continue
		}
		rest, body, next := section(lines, i)
		switch k {
		case "on", "true": // YAML 1.1 reads a bare `on` as true
			triggers, triggersKnown = parseTriggers(rest, body)
		case "jobs":
			jobs = parseJobs(body, func(n, r, e string, u bool) job { return job{n, r, e, u} })
		}
		i = next
	}

	var findings []Finding
	local := 0
	prSeen := false
	for _, t := range triggers {
		if t == "pull_request" {
			prSeen = true
		}
	}
	var localJobs []job
	for _, j := range jobs {
		if j.hasUses && j.runsOn == "" {
			continue // a reusable-workflow call: the called file is scanned on its own
		}
		if mayRunLocally(j.runsOn, p.LocalLabels) {
			local++
			localJobs = append(localJobs, j)
		}
	}
	if local == 0 {
		return len(jobs), 0, nil
	}

	if !triggersKnown {
		findings = append(findings, Finding{File: name,
			Message: "its triggers cannot be determined, and a job in it may run on the local runner",
			Fix:     "write `on:` as a block or a simple list (on: [push, pull_request])"})
	}
	for _, t := range triggers {
		if !allowedTriggers[t] {
			findings = append(findings, Finding{File: name,
				Message: fmt.Sprintf("trigger %q can be fired by people outside your control, and this workflow has a job that may run on the local runner", t),
				Fix:     "move the local job to a workflow that only uses push, pull_request, workflow_dispatch, schedule or merge_group, or make it run on a GitHub-hosted image"})
		}
	}
	for _, j := range localJobs {
		if reason := CheckIf(j.ifExpr, p.TrustedActors, prSeen); reason != "" {
			findings = append(findings, Finding{File: name, Job: j.name, Message: reason,
				Fix: "add to the job:  if: " + CanonicalIf(p.TrustedActors[:min(1, len(p.TrustedActors))], prSeen)})
		}
	}
	return len(jobs), local, findings
}

func parseTriggers(rest string, body []line) ([]string, bool) {
	clean := func(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }
	switch {
	case strings.HasPrefix(rest, "{"):
		return nil, false
	case strings.HasPrefix(rest, "["):
		inner := strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]")
		var out []string
		for _, p := range strings.Split(inner, ",") {
			if c := clean(p); c != "" {
				out = append(out, c)
			}
		}
		return out, len(out) > 0
	case rest != "":
		return []string{clean(rest)}, true
	}
	if len(body) == 0 {
		return nil, false
	}
	base := body[0].indent
	var out []string
	for _, l := range body {
		if l.indent != base {
			continue
		}
		if strings.HasPrefix(l.text, "- ") {
			out = append(out, clean(strings.TrimPrefix(l.text, "- ")))
		} else if k, _, ok := key(l.text); ok {
			out = append(out, k)
		}
	}
	return out, len(out) > 0
}

func parseJobs[T any](body []line, mk func(name, runsOn, ifExpr string, uses bool) T) []T {
	if len(body) == 0 {
		return nil
	}
	base := body[0].indent
	var out []T
	for i := 0; i < len(body); {
		name, _, ok := key(body[i].text)
		if body[i].indent != base || !ok {
			i++
			continue
		}
		j := i + 1
		for j < len(body) && body[j].indent > base {
			j++
		}
		block := body[i+1 : j]
		runsOn, ifExpr, uses := jobProps(block)
		out = append(out, mk(name, runsOn, ifExpr, uses))
		i = j
	}
	return out
}

// jobProps reads runs-on, if and uses from a job's own properties (not its steps).
func jobProps(block []line) (runsOn, ifExpr string, uses bool) {
	if len(block) == 0 {
		return
	}
	base := block[0].indent
	for i := 0; i < len(block); i++ {
		if block[i].indent != base {
			continue
		}
		k, rest, ok := key(block[i].text)
		if !ok {
			continue
		}
		var cont []string
		for j := i + 1; j < len(block) && block[j].indent > base; j++ {
			cont = append(cont, block[j].text)
		}
		val := strings.TrimSpace(rest + " " + strings.Join(cont, " "))
		if strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			val = strings.Join(cont, " ")
		}
		switch k {
		case "runs-on":
			runsOn = val
		case "if":
			ifExpr = val
		case "uses":
			uses = true
		}
	}
	return
}

// mayRunLocally is true unless runs-on is made only of plain GitHub-hosted image names.
// An expression, a mapping (group/labels), or any local label counts as possibly local.
func mayRunLocally(runsOn string, local []string) bool {
	if strings.TrimSpace(runsOn) == "" {
		return true // no runs-on we can read: assume it can
	}
	if strings.Contains(runsOn, "${{") {
		return true
	}
	isLocal := map[string]bool{"self-hosted": true}
	for _, l := range local {
		isLocal[strings.ToLower(l)] = true
	}
	tokens := strings.FieldsFunc(runsOn, func(r rune) bool {
		return !(r == '-' || r == '.' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	for _, t := range tokens {
		lt := strings.ToLower(t)
		switch {
		case lt == "group" || lt == "labels":
			return true // runner groups and label lists can name self-hosted runners
		case isLocal[lt]:
			return true
		case !hostedRe.MatchString(lt):
			return true
		}
	}
	return false
}
