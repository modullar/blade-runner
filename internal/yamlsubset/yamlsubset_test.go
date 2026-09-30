package yamlsubset

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSpecExample(t *testing.T) {
	src := `# bladerunner.yaml  - committed to the project, contains no secrets
version: 1
bladerunner:
  min_version: "0.1.0"          # doctor warns if the installed CLI is older
runner:
  scope: repo                   # repo | org
  repository: OWNER/REPO
  labels: [self-hosted, macOS, ARM64]
  work_dir: ~/.bladerunner/work
  token:
    source: keychain
placement:
  default: auto
  jobs:
    unit-tests: local
    e2e:        github          # offload
workflows:
  - .github/workflows/ci.yml
agent:
  listen: 127.0.0.1:7878
`
	root, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := root.Map["version"].Value; got != "1" {
		t.Errorf("version = %q", got)
	}
	if got := root.Map["bladerunner"].Map["min_version"]; got.Value != "0.1.0" || !got.Quoted {
		t.Errorf("min_version = %+v", got)
	}
	labels := root.Map["runner"].Map["labels"]
	var got []string
	for _, it := range labels.Items {
		got = append(got, it.Value)
	}
	if want := []string{"self-hosted", "macOS", "ARM64"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	jobs := root.Map["placement"].Map["jobs"]
	if !reflect.DeepEqual(jobs.Keys, []string{"unit-tests", "e2e"}) || jobs.Map["e2e"].Value != "github" {
		t.Errorf("jobs = %+v", jobs)
	}
	if got := root.Map["workflows"].Items[0].Value; got != ".github/workflows/ci.yml" {
		t.Errorf("workflow = %q", got)
	}
	if got := root.Map["agent"].Map["listen"].Value; got != "127.0.0.1:7878" {
		t.Errorf("listen = %q (a colon inside a value must survive)", got)
	}
}

func TestParseForms(t *testing.T) {
	tests := []struct {
		name string
		src  string
		path []string
		want string
	}{
		{"leading document marker", "---\na: b\n", []string{"a"}, "b"},
		{"single quoted with escaped quote", "a: 'it''s'\n", []string{"a"}, "it's"},
		{"double quoted escapes", `a: "x\"y\\z"` + "\n", []string{"a"}, `x"y\z`},
		{"hash inside quotes is not a comment", `a: "x # y"` + "\n", []string{"a"}, "x # y"},
		{"apostrophe inside plain word", "a: don't # c\n", []string{"a"}, "don't"},
		{"same-indent list", "l:\n- x\n- y\nk: v\n", []string{"k"}, "v"},
		{"CRLF", "a: b\r\nc: d\r\n", []string{"c"}, "d"},
		{"quoted key", `"a b": c` + "\n", []string{"a b"}, "c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range tc.path {
				n = n.Map[k]
			}
			if n.Value != tc.want {
				t.Errorf("got %q, want %q", n.Value, tc.want)
			}
		})
	}
}

func TestParseEmptyValuesAndDocument(t *testing.T) {
	root, err := Parse([]byte("a:\nb: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if root.Map["a"].Kind != Null {
		t.Errorf("a.Kind = %v, want Null", root.Map["a"].Kind)
	}
	if b := root.Map["b"]; b.Kind != List || len(b.Items) != 0 {
		t.Errorf("b = %+v", b)
	}
	empty, err := Parse([]byte("# only a comment\n\n"))
	if err != nil || empty.Kind != Map || len(empty.Keys) != 0 {
		t.Errorf("empty doc = %+v, %v", empty, err)
	}
}

func TestParseRefusesWhatItDoesNotSupport(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
		want string
	}{
		{"tab indent", "a:\n\tb: c\n", 2, "tabs"},
		{"anchor", "a: &x 1\n", 1, "unsupported YAML syntax"},
		{"alias", "a: *x\n", 1, "unsupported YAML syntax"},
		{"block scalar", "a: |\n  text\n", 1, "unsupported YAML syntax"},
		{"flow map", "a: {b: c}\n", 1, "unsupported YAML syntax"},
		{"tag", "a: !!str x\n", 1, "unsupported YAML syntax"},
		{"second document", "a: b\n---\nc: d\n", 2, "multiple documents"},
		{"list of maps", "l:\n  - name: x\n", 2, "lists of mappings"},
		{"duplicate key", "a: 1\na: 2\n", 2, "duplicate key"},
		{"multi-line scalar", "a: b\n  c\n", 2, "multi-line"},
		{"bad indentation", "a: 1\n  b: 2\n", 2, "multi-line"},
		{"not key value", "a: 1\njust text\n", 2, "expected"},
		{"unterminated quote", `a: "x` + "\n", 1, "unterminated"},
		{"multi-line flow list", "a: [x,\n  y]\n", 1, "one-line flow list"},
		{"nested flow", "a: [[x]]\n", 1, "nested"},
		{"empty flow item", "a: [x, , y]\n", 1, "empty item"},
		{"unknown escape", `a: "\q"` + "\n", 1, "unsupported escape"},
		{"nested list", "l:\n  - [x]\n", 2, "nested lists"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			pe, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is %T, want *Error", err)
			}
			if pe.Line != tc.line {
				t.Errorf("line = %d, want %d (%v)", pe.Line, tc.line, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestQuoteRoundTrips(t *testing.T) {
	for _, s := range []string{
		"plain", "0.1.0", "true", "yes", "", "has space", `with "quote"`, "a: b", "x # y",
		"- dash", "127.0.0.1:7878", "OWNER/REPO", "~/.bladerunner/work", "123", "line\nbreak", `back\slash`,
		"null", "ends:",
	} {
		root, err := Parse([]byte("k: " + Quote(s) + "\n"))
		if err != nil {
			t.Errorf("Quote(%q) = %s does not parse: %v", s, Quote(s), err)
			continue
		}
		if got := root.Map["k"].Value; got != s {
			t.Errorf("round trip of %q gave %q (rendered %s)", s, got, Quote(s))
		}
	}
	// Ambiguous plain forms must be quoted so a reader cannot take them for another type.
	for _, s := range []string{"0.1.0", "true", "123", ""} {
		if !strings.HasPrefix(Quote(s), `"`) {
			t.Errorf("Quote(%q) = %s, want a quoted form", s, Quote(s))
		}
	}
}
