package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Prompt history is kept per chat under the state directory
// (<state>/tui/history/<chatID>): one JSON string per line, so a prompt
// with line breaks survives, appended as prompts are sent and trimmed to
// the newest historyCap when it grows past twice that. The chat service
// never reads it; it is the terminal client's own state.

const historyCap = 500

var historyID = strings.NewReplacer("/", "_", "\\", "_", "..", "_")

// historyFile is where chatID's prompts live under dir.
func historyFile(dir, chatID string) string {
	return filepath.Join(dir, historyID.Replace(chatID))
}

// LoadHistory reads a chat's recorded prompts, oldest first; a missing
// file is an empty history.
func LoadHistory(dir, chatID string) []string {
	if dir == "" || chatID == "" {
		return nil
	}
	f, err := os.Open(historyFile(dir, chatID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var s string
		if json.Unmarshal(sc.Bytes(), &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	if len(out) > historyCap {
		out = out[len(out)-historyCap:]
	}
	return out
}

// AppendHistory records one prompt for the chat, skipping a repeat of the
// last one, and compacts the file when it has grown past twice the cap.
func AppendHistory(dir, chatID, prompt string) error {
	if dir == "" || chatID == "" || strings.TrimSpace(prompt) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := historyFile(dir, chatID)
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if last := lines[len(lines)-1]; last != "" {
			var s string
			if json.Unmarshal([]byte(last), &s) == nil && s == prompt {
				return nil
			}
		}
		if len(lines) >= 2*historyCap {
			kept := lines[len(lines)-historyCap+1:]
			if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0600); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(prompt)
	_, err = f.Write(append(line, '\n'))
	return err
}

// SearchHistory finds the newest prompt before index `before` (len(h) to
// start from the end) that contains query, case-insensitively; -1 when
// none does. An empty query matches the previous prompt.
func SearchHistory(h []string, query string, before int) int {
	needle := strings.ToLower(query)
	for i := min(before, len(h)) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(h[i]), needle) {
			return i
		}
	}
	return -1
}
