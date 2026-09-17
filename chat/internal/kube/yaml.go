package kube

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseYAML parses the YAML subset that kubeconfig files written by kubectl,
// k3s and the dev scripts use: block mappings, block sequences (of scalars
// or of mappings, at the parent key's indentation or deeper), plain,
// single-quoted and double-quoted scalars, the empty flow collections {}
// and [], null/~, comments and the --- document marker. Every scalar is
// returned as a string (or nil for null); the caller interprets booleans.
// Block scalars (| and >), non-empty flow collections, anchors, aliases,
// tags and multi-document files are refused with an error naming the line.
func parseYAML(data []byte) (any, error) {
	lines, err := yamlLines(string(data))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	p := &yamlParser{lines: lines}
	value, err := p.node(0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(lines) {
		return nil, p.errorf(p.pos, "unexpected content")
	}
	return value, nil
}

// parseJSONDocument parses a JSON kubeconfig into the same shape parseYAML
// produces, except that booleans and numbers keep their JSON types.
func parseJSONDocument(data []byte) (any, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return doc, nil
}

type yamlLine struct {
	number int // 1-based, for errors
	indent int
	text   string // comment stripped, right-trimmed
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

func (p *yamlParser) errorf(pos int, format string, args ...any) error {
	number := 0
	if pos < len(p.lines) {
		number = p.lines[pos].number
	} else if len(p.lines) > 0 {
		number = p.lines[len(p.lines)-1].number
	}
	return fmt.Errorf("yaml line %d: %s", number, fmt.Sprintf(format, args...))
}

// yamlLines splits the document into meaningful lines.
func yamlLines(s string) ([]yamlLine, error) {
	var out []yamlLine
	documents := 0
	for i, raw := range strings.Split(s, "\n") {
		raw = strings.TrimRight(raw, " \t\r")
		if raw == "" || strings.HasPrefix(raw, "%") {
			continue
		}
		if raw == "---" || strings.HasPrefix(raw, "--- ") {
			documents++
			if documents > 1 || len(out) > 0 {
				return nil, fmt.Errorf("yaml line %d: multi-document files are not supported", i+1)
			}
			continue
		}
		if raw == "..." {
			break
		}
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		if indent < len(raw) && raw[indent] == '\t' {
			return nil, fmt.Errorf("yaml line %d: tabs are not allowed in indentation", i+1)
		}
		text := stripComment(raw[indent:])
		if text == "" {
			continue
		}
		out = append(out, yamlLine{number: i + 1, indent: indent, text: text})
	}
	return out, nil
}

// stripComment removes a trailing comment: a '#' outside quotes that starts
// the text or follows whitespace.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'' && c == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
			} else {
				quote = 0
			}
		case quote == '"' && c == '\\':
			i++
		case quote == '"' && c == '"':
			quote = 0
		case quote != 0:
		case c == '\'' || c == '"':
			if i == 0 || s[i-1] == ' ' || s[i-1] == ':' || s[i-1] == '-' || s[i-1] == '[' || s[i-1] == ',' {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return strings.TrimRight(s[:i], " ")
		}
	}
	return strings.TrimRight(s, " ")
}

// node parses the block starting at line p.pos, which must be at indent.
func (p *yamlParser) node(pos, indent int) (any, error) {
	p.pos = pos
	if pos >= len(p.lines) {
		return nil, nil
	}
	line := p.lines[pos]
	if line.indent != indent {
		return nil, p.errorf(pos, "bad indentation")
	}
	if isSequenceItem(line.text) {
		return p.sequence(indent)
	}
	if _, _, ok := splitKey(line.text); ok {
		return p.mapping(indent)
	}
	value, err := scalar(line.text)
	if err != nil {
		return nil, p.errorf(pos, "%v", err)
	}
	p.pos = pos + 1
	return value, nil
}

func isSequenceItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

func (p *yamlParser) sequence(indent int) (any, error) {
	items := []any{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, p.errorf(p.pos, "bad indentation")
		}
		if !isSequenceItem(line.text) {
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(line.text, "-"), " ")
		if rest == "" {
			next := p.pos + 1
			if next < len(p.lines) && p.lines[next].indent > indent {
				item, err := p.node(next, p.lines[next].indent)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			} else {
				items = append(items, nil)
				p.pos = next
			}
			continue
		}
		offset := len(line.text) - len(rest)
		if _, _, ok := splitKey(rest); ok && !isQuoted(rest) {
			// The item is a mapping whose first entry shares the dash's
			// line; parse it as if it began on its own line at the
			// entry's column.
			p.lines[p.pos] = yamlLine{number: line.number, indent: indent + offset, text: rest}
			item, err := p.mapping(indent + offset)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
			continue
		}
		if isSequenceItem(rest) {
			return nil, p.errorf(p.pos, "nested sequences on one line are not supported")
		}
		value, err := scalar(rest)
		if err != nil {
			return nil, p.errorf(p.pos, "%v", err)
		}
		items = append(items, value)
		p.pos++
	}
	return items, nil
}

func (p *yamlParser) mapping(indent int) (any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, p.errorf(p.pos, "bad indentation")
		}
		if isSequenceItem(line.text) {
			break
		}
		key, rest, ok := splitKey(line.text)
		if !ok {
			return nil, p.errorf(p.pos, "expected a key")
		}
		if _, dup := m[key]; dup {
			return nil, p.errorf(p.pos, "duplicate key %q", key)
		}
		if rest != "" {
			value, err := scalar(rest)
			if err != nil {
				return nil, p.errorf(p.pos, "%v", err)
			}
			m[key] = value
			p.pos++
			continue
		}
		next := p.pos + 1
		switch {
		case next < len(p.lines) && p.lines[next].indent > indent:
			value, err := p.node(next, p.lines[next].indent)
			if err != nil {
				return nil, err
			}
			m[key] = value
		case next < len(p.lines) && p.lines[next].indent == indent && isSequenceItem(p.lines[next].text):
			// A sequence at its key's indentation, as kubectl writes.
			p.pos = next
			value, err := p.sequence(indent)
			if err != nil {
				return nil, err
			}
			m[key] = value
		default:
			m[key] = nil
			p.pos = next
		}
	}
	return m, nil
}

// splitKey splits "key: value" or "key:" at the first colon outside quotes
// that ends the key.
func splitKey(text string) (key, rest string, ok bool) {
	if text == "" || text[0] == '[' || text[0] == '{' {
		return "", "", false
	}
	if text[0] == '"' || text[0] == '\'' {
		end := closingQuote(text)
		if end < 0 || end+1 >= len(text) || text[end+1] != ':' {
			return "", "", false
		}
		if end+2 < len(text) && text[end+2] != ' ' {
			return "", "", false
		}
		k, err := scalar(text[:end+1])
		if err != nil {
			return "", "", false
		}
		key, _ = k.(string)
		return key, strings.TrimSpace(text[end+2:]), true
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ') {
			key = strings.TrimSpace(text[:i])
			if key == "" {
				return "", "", false
			}
			return key, strings.TrimSpace(text[i+1:]), true
		}
	}
	return "", "", false
}

func isQuoted(s string) bool { return s != "" && (s[0] == '"' || s[0] == '\'') }

// closingQuote returns the index of the quote that closes the one at s[0].
func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '\'' && s[i] == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i
		case q == '"' && s[i] == '\\':
			i++
		case q == '"' && s[i] == '"':
			return i
		}
	}
	return -1
}

// scalar interprets one scalar value.
func scalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	case "{}":
		return map[string]any{}, nil
	case "[]":
		return []any{}, nil
	}
	switch s[0] {
	case '\'':
		end := closingQuote(s)
		if end != len(s)-1 {
			return nil, errors.New("unterminated single-quoted string")
		}
		return strings.ReplaceAll(s[1:end], "''", "'"), nil
	case '"':
		end := closingQuote(s)
		if end != len(s)-1 {
			return nil, errors.New("unterminated double-quoted string")
		}
		value, err := strconv.Unquote(s)
		if err != nil {
			return nil, errors.New("invalid double-quoted string")
		}
		return value, nil
	case '|', '>':
		return nil, errors.New("block scalars are not supported")
	case '{', '[':
		return nil, errors.New("flow collections are not supported")
	case '&', '*', '!':
		return nil, errors.New("anchors, aliases and tags are not supported")
	}
	return s, nil
}
