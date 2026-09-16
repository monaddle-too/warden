package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxAPIKeyUsesStdinAndScopedCleanup(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "sbx")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + filepath.Join(root, "args") + "'\nif [ \"$1 $2\" = 'secret set' ]; then cat > '" + filepath.Join(root, "stdin") + "'; fi\nif [ \"$1\" = stop ] && [ -f '" + filepath.Join(root, "fail-stop") + "' ]; then exit 1; fi\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(root, script, "test")
	key := "sk-fixture-secret-must-stay-on-host"
	res, cleanup, err := w.prepareOpenAI(context.Background(), Request{SessionID: "session-one", OpenAIAPIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if res.APIKeyPlaceholder == key || res.APIKeyPlaceholder == "" {
		t.Fatal("real key entered guest")
	}
	b, _ := os.ReadFile(filepath.Join(root, "stdin"))
	if string(b) != key+"\n" {
		t.Fatal("key not supplied on stdin")
	}
	b, _ = os.ReadFile(filepath.Join(root, "args"))
	if strings.Contains(string(b), key) || !strings.Contains(string(b), "secret\nset\nopenai\n--sandbox\nws-session-one\n") {
		t.Fatal("key exposed in argv or missing scope")
	}
	b, _ = os.ReadFile(w.openAIMarker("session-one"))
	if strings.Contains(string(b), key) {
		t.Fatal("key persisted in recovery marker")
	}
	// Failed shutdown must not restore global OAuth under a still-running guest.
	if err = os.WriteFile(filepath.Join(root, "fail-stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cleanup()
	b, _ = os.ReadFile(filepath.Join(root, "args"))
	if strings.Contains(string(b), "\nrm\n") {
		t.Fatal("restored owner auth before guest stopped")
	}
	if _, err = os.Stat(w.openAIMarker("session-one")); err != nil {
		t.Fatal("lost recovery marker")
	}
	if err = os.Remove(filepath.Join(root, "fail-stop")); err != nil {
		t.Fatal(err)
	}
	// A fresh worker recovers after a crash or failed teardown.
	restarted := NewWorker(root, script, "test")
	if err = restarted.recoverOpenAIRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(w.openAIMarker("session-one")); !os.IsNotExist(err) {
		t.Fatal("cleanup marker remains")
	}
	b, _ = os.ReadFile(filepath.Join(root, "args"))
	if !strings.Contains(string(b), "stop\nws-session-one\nsecret\nrm\nopenai\n--sandbox\nws-session-one\n--force\n") {
		t.Fatal("missing scoped teardown")
	}
	if err = restarted.recoverOpenAIRuns(context.Background()); err != nil {
		t.Fatal("recovery not idempotent", err)
	}
}
