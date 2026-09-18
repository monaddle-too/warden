package tui

import (
	"encoding/json"
	"os"
	"testing"
)

// The cases the web's activity.ts runs too (activity.test.ts), so both
// surfaces say the same words for the same transcript.
func TestActivityLabelSharedCases(t *testing.T) {
	b, err := os.ReadFile("testdata/activity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string  `json:"name"`
		Entries []Entry `json:"entries"`
		Want    string  `json:"want"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		if got := ActivityLabel(c.Entries); got != c.Want {
			t.Errorf("%s: got %q, want %q", c.Name, got, c.Want)
		}
	}
	if RunningLabel(nil) != "Agent is working" {
		t.Fatal("fallback")
	}
}

func TestActivityWords(t *testing.T) {
	if got := trimCommand("  go   test\t./...\nsleep 1\n", 48); got != "go test ./..." {
		t.Fatalf("trimCommand: %q", got)
	}
	if got := trimCommand("xxxxxxxxxxxxxxxxxxxx", 10); got != "xxxxxxxxx…" {
		t.Fatalf("cut: %q", got)
	}
	for in, want := range map[string]string{"/home/agent/workspace/chat/a.go": "a.go", "a.go": "a.go", "/home/agent/workspace/chat/": "chat"} {
		if got := basename(in); got != want {
			t.Fatalf("basename(%q) = %q", in, got)
		}
	}
	if host("https://example.com/x?y") != "example.com" || host("nope") != "nope" {
		t.Fatal("host")
	}
}
