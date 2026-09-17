package kube

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseYAMLKubeconfigShapes(t *testing.T) {
	doc, err := parseYAML([]byte(`---
# kubectl style: sequences at the key's indentation
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTg==
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion": "v1",
		"clusters": []any{map[string]any{
			"cluster": map[string]any{"certificate-authority-data": "LS0tLS1CRUdJTg==", "server": "https://127.0.0.1:6443"},
			"name":    "default",
		}},
		"contexts": []any{map[string]any{
			"context": map[string]any{"cluster": "default", "user": "default"},
			"name":    "default",
		}},
		"current-context": "default",
		"kind":            "Config",
		"preferences":     map[string]any{},
		"users": []any{map[string]any{
			"name": "default",
			"user": map[string]any{"client-certificate-data": "Q0VSVA==", "client-key-data": "S0VZ"},
		}},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("got %#v\nwant %#v", doc, want)
	}
}

func TestParseYAMLIndentedSequencesScalarsAndComments(t *testing.T) {
	doc, err := parseYAML([]byte("items:\n  - name: a   # trailing comment\n    value: 'single # not a comment'\n    other: \"double \\\"quoted\\\" with # hash\"\n  - name: b\n    nested:\n      - x\n      - y\n      -\n      - ~\n    empty: []\n    none: null\n    tilde: ~\n    blank:\n    number: 6443\n    url: http://x:1/y#frag\n  -\n    late: true\r\nscalar: value: with colons\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"items": []any{
			map[string]any{"name": "a", "value": "single # not a comment", "other": `double "quoted" with # hash`},
			map[string]any{"name": "b", "nested": []any{"x", "y", nil, nil}, "empty": []any{}, "none": nil, "tilde": nil, "blank": nil, "number": "6443", "url": "http://x:1/y#frag"},
			map[string]any{"late": "true"},
		},
		"scalar": "value: with colons",
	}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("got %#v\nwant %#v", doc, want)
	}
	// Quoted keys and a top-level sequence.
	doc, err = parseYAML([]byte("\"quoted key\": 1\n'single key': 2\nplain: 'it''s'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, map[string]any{"quoted key": "1", "single key": "2", "plain": "it's"}) {
		t.Fatalf("got %#v", doc)
	}
	doc, err = parseYAML([]byte("- a\n- b: c\n  d: e\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, []any{"a", map[string]any{"b": "c", "d": "e"}}) {
		t.Fatalf("got %#v", doc)
	}
	if doc, err := parseYAML([]byte("\n# only a comment\n")); err != nil || doc != nil {
		t.Fatalf("empty document: %#v %v", doc, err)
	}
	if doc, err := parseYAML([]byte("just a scalar\n")); err != nil || doc != "just a scalar" {
		t.Fatalf("scalar document: %#v %v", doc, err)
	}
}

func TestParseYAMLRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"a:\n\tb: 1\n":           "tabs are not allowed",
		"a: |\n  block\n":        "block scalars",
		"a: >-\n  folded\n":      "block scalars",
		"a: {b: c}\n":            "flow collections",
		"a: [b, c]\n":            "flow collections",
		"a: &anchor b\n":         "anchors",
		"a: *alias\n":            "anchors",
		"a: !!str b\n":           "anchors, aliases and tags",
		"a: 1\na: 2\n":           "duplicate key",
		"a: 'unterminated\n":     "unterminated single",
		"a: \"unterminated\n":    "unterminated double",
		"a:\n  b: 1\n c: 2\n":    "bad indentation",
		"- a\n  - b\n":           "bad indentation",
		"- - a\n":                "nested sequences",
		"---\na: 1\n---\nb: 2\n": "multi-document",
		"a: 1\n---\nb: 2\n":      "multi-document",
		"a: 1\nb\n":              "expected a key",
	}
	for input, want := range cases {
		_, err := parseYAML([]byte(input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", input, err, want)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "yaml line ") {
			t.Errorf("%q: error does not name the line: %v", input, err)
		}
	}
}
