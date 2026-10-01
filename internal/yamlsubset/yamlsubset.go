// Package yamlsubset parses the small, strict subset of YAML that bladerunner.yaml uses,
// with the standard library only.
//
// Supported: block mappings, block sequences of scalars, one-line flow sequences of
// scalars ([a, "b"]), plain / 'single' / "double" quoted scalars, # comments, an optional
// leading "---". Anything else (anchors, aliases, tags, block scalars, flow mappings,
// multi-line scalars, lists of mappings, tabs, duplicate keys, extra documents) is an
// error naming its line: a config parser that guesses is worse than one that refuses.
package yamlsubset

import (
	"fmt"
	"strings"
)

// Kind is the type of a Node.
type Kind int

const (
	Null Kind = iota
	Scalar
	Map
	List
)

// Node is one parsed value.
type Node struct {
	Kind   Kind
	Line   int
	Value  string // Scalar
	Quoted bool   // Scalar was quoted in the source
	Keys   []string
	Map    map[string]*Node
	Items  []*Node
}

// Error is a syntax error with its 1-based line.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

func errAt(line int, format string, args ...any) *Error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

type line struct {
	n      int    // 1-based source line
	indent int    // leading spaces
	text   string // content, comment and trailing space removed
}

// Parse parses src. An empty document is a Map with no keys.
func Parse(src []byte) (*Node, error) {
	lines, err := tokenize(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &Node{Kind: Map, Line: 1, Map: map[string]*Node{}}, nil
	}
	p := &parser{lines: lines}
	root, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.i < len(lines) {
		return nil, errAt(lines[p.i].n, "unexpected indentation")
	}
	return root, nil
}

func tokenize(src string) ([]line, error) {
	var out []line
	seenContent := false
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		n := i + 1
		if i == 0 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		text := strings.TrimRight(stripComment(raw), " \t")
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := 0
		for indent < len(text) && text[indent] == ' ' {
			indent++
		}
		if indent < len(text) && text[indent] == '\t' {
			return nil, errAt(n, "tabs are not allowed for indentation")
		}
		body := text[indent:]
		switch {
		case body == "---" && indent == 0:
			if seenContent {
				return nil, errAt(n, "multiple documents are not supported")
			}
			continue
		case body == "..." || strings.HasPrefix(body, "--- "):
			return nil, errAt(n, "document markers other than a leading \"---\" are not supported")
		}
		seenContent = true
		out = append(out, line{n: n, indent: indent, text: body})
	}
	return out, nil
}

// stripComment removes a # comment that is outside quotes and starts a token.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && opensToken(s, i):
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// opensToken reports whether the quote at s[i] can begin a quoted scalar (as opposed to an
// apostrophe inside a plain word like "don't").
func opensToken(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case ' ', '\t', '[', ',', ':':
		return true
	}
	return false
}

type parser struct {
	lines []line
	i     int
}

func (p *parser) block(indent int) (*Node, error) {
	l := p.lines[p.i]
	if l.text == "-" || strings.HasPrefix(l.text, "- ") {
		return p.list(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (*Node, error) {
	node := &Node{Kind: Map, Line: p.lines[p.i].n, Map: map[string]*Node{}}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errAt(l.n, "unexpected indentation")
		}
		if l.text == "-" || strings.HasPrefix(l.text, "- ") {
			return nil, errAt(l.n, "a list item where a \"key: value\" line was expected")
		}
		key, rest, err := splitKey(l)
		if err != nil {
			return nil, err
		}
		if _, dup := node.Map[key]; dup {
			return nil, errAt(l.n, "duplicate key %q", key)
		}
		p.i++
		var child *Node
		if rest == "" {
			child, err = p.nested(indent, l.n)
		} else {
			child, err = parseInline(rest, l.n)
			if err == nil && p.i < len(p.lines) && p.lines[p.i].indent > indent {
				err = errAt(p.lines[p.i].n, "multi-line scalars are not supported")
			}
		}
		if err != nil {
			return nil, err
		}
		node.Keys = append(node.Keys, key)
		node.Map[key] = child
	}
	return node, nil
}

// nested parses the value under a bare "key:" line: an indented block, a same-indent list,
// or null.
func (p *parser) nested(parentIndent, keyLine int) (*Node, error) {
	if p.i >= len(p.lines) {
		return &Node{Kind: Null, Line: keyLine}, nil
	}
	next := p.lines[p.i]
	switch {
	case next.indent > parentIndent:
		return p.block(next.indent)
	case next.indent == parentIndent && (next.text == "-" || strings.HasPrefix(next.text, "- ")):
		return p.list(parentIndent)
	}
	return &Node{Kind: Null, Line: keyLine}, nil
}

func (p *parser) list(indent int) (*Node, error) {
	node := &Node{Kind: List, Line: p.lines[p.i].n}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errAt(l.n, "unexpected indentation")
		}
		if l.text != "-" && !strings.HasPrefix(l.text, "- ") {
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		if item == "" {
			return nil, errAt(l.n, "empty list items are not supported")
		}
		if k, _, ok := findKeyColon(item); ok && !strings.HasPrefix(item, "\"") && !strings.HasPrefix(item, "'") {
			return nil, errAt(l.n, "lists of mappings are not supported (item looks like %q)", item[:k]+":")
		}
		child, err := parseInline(item, l.n)
		if err != nil {
			return nil, err
		}
		if child.Kind == List {
			return nil, errAt(l.n, "nested lists are not supported")
		}
		node.Items = append(node.Items, child)
		p.i++
	}
	return node, nil
}

func splitKey(l line) (key, rest string, err error) {
	k, r, ok := findKeyColon(l.text)
	if !ok {
		return "", "", errAt(l.n, "expected \"key: value\", got %q", l.text)
	}
	key = strings.TrimSpace(l.text[:k])
	if strings.HasPrefix(key, "\"") || strings.HasPrefix(key, "'") {
		n, err := parseScalar(key, l.n)
		if err != nil {
			return "", "", err
		}
		key = n.Value
	}
	if key == "" {
		return "", "", errAt(l.n, "empty key")
	}
	return key, strings.TrimSpace(l.text[r:]), nil
}

// findKeyColon locates the first ':' outside quotes that is followed by a space or the end.
// It returns the colon's index and the index just past it.
func findKeyColon(s string) (colon, rest int, ok bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && opensToken(s, i):
			quote = c
		case c == ':' && (i+1 == len(s) || s[i+1] == ' '):
			return i, i + 1, true
		}
	}
	return 0, 0, false
}

func parseInline(s string, n int) (*Node, error) {
	if strings.HasPrefix(s, "[") {
		return parseFlowList(s, n)
	}
	return parseScalar(s, n)
}

func parseFlowList(s string, n int) (*Node, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, errAt(n, "a value starting with '[' must be a complete one-line flow list; quote it (\"...\") if it is text, as IPv6 addresses like \"[::1]:7878\" must be")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	node := &Node{Kind: List, Line: n}
	if inner == "" {
		return node, nil
	}
	var parts []string
	var quote byte
	start := 0
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case quote != 0:
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && opensToken(inner, i):
			quote = c
		case c == '[' || c == '{':
			return nil, errAt(n, "nested flow collections are not supported")
		case c == ',':
			parts = append(parts, inner[start:i])
			start = i + 1
		}
	}
	parts = append(parts, inner[start:])
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errAt(n, "empty item in flow list")
		}
		item, err := parseScalar(part, n)
		if err != nil {
			return nil, err
		}
		node.Items = append(node.Items, item)
	}
	return node, nil
}

func parseScalar(s string, n int) (*Node, error) {
	s = strings.TrimSpace(s)
	switch s[0] {
	case '"':
		v, err := unquoteDouble(s, n)
		if err != nil {
			return nil, err
		}
		return &Node{Kind: Scalar, Line: n, Value: v, Quoted: true}, nil
	case '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return nil, errAt(n, "unterminated single-quoted string")
		}
		return &Node{Kind: Scalar, Line: n, Value: strings.ReplaceAll(s[1:len(s)-1], "''", "'"), Quoted: true}, nil
	case '&', '*', '!', '|', '>', '{', '@', '%', '`':
		return nil, errAt(n, "unsupported YAML syntax %q (anchors, aliases, tags, block scalars and flow maps are not supported)", string(s[0]))
	}
	return &Node{Kind: Scalar, Line: n, Value: s}, nil
}

func unquoteDouble(s string, n int) (string, error) {
	if len(s) < 2 || s[len(s)-1] != '"' {
		return "", errAt(n, "unterminated double-quoted string")
	}
	var b strings.Builder
	body := s[1 : len(s)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", errAt(n, "dangling backslash in double-quoted string")
		}
		switch body[i] {
		case '\\', '"', '/':
			b.WriteByte(body[i])
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			return "", errAt(n, "unsupported escape \\%c in double-quoted string", body[i])
		}
	}
	return b.String(), nil
}

// Quote renders s as a scalar that Parse reads back unchanged: plain when that is
// unambiguous, double-quoted otherwise.
func Quote(s string) string {
	if isPlainSafe(s) {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func isPlainSafe(s string) bool {
	if s == "" || s[0] == '-' || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == '/' || c == '~' || c == '-' || c == ':' || c == '@'
		if !ok {
			return false
		}
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return false
	}
	if looksNumeric(s) || strings.HasSuffix(s, ":") {
		return false
	}
	return true
}

func looksNumeric(s string) bool {
	digits := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' || c == '_':
		default:
			return false
		}
	}
	return digits > 0
}
