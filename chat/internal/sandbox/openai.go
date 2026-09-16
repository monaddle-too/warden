package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (w *Worker) openAIMarker(id string) string {
	return filepath.Join(w.Root, "openai-runs", strings.ToLower(id)+".json")
}

// SBX's service credential store overrides the workspace OAuth connection.
// Pass the key on stdin, never in argv or into the guest. Dynamic command
// resolvers on pinned SBX can silently fall back to OAuth, so do not use them.
func (w *Worker) prepareOpenAI(ctx context.Context, r Request) (Response, func(), error) {
	if r.OpenAIAPIKey == "" {
		return Response{}, nil, errors.New("OpenAI API key required")
	}
	// Record cleanup intent before registering the secret, so a worker crash
	// cannot leave a personal key attached to a later workspace-default run.
	if err := atomicJSON(w.openAIMarker(r.SessionID), true); err != nil {
		return Response{}, nil, err
	}
	cleanup := sync.OnceFunc(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.recoverOpenAI(cleanupCtx, r.SessionID)
	})
	name := "ws-" + strings.ToLower(r.SessionID)
	cmd := command(ctx, w.Executable, "secret", "set", "openai", "--sandbox", name)
	cmd.Stdin = strings.NewReader(r.OpenAIAPIKey + "\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		cleanup()
		return Response{}, nil, errors.New("Could not attach your API key to this sandbox; no agent was started.")
	}
	return Response{APIKeyPlaceholder: "sk-proj-proxy-managed"}, cleanup, nil
}

// Called with exclusive session admission, or at startup after managed VMs stop.
// Leave the marker on any failure so the next admission retries and fails closed.
func (w *Worker) recoverOpenAI(ctx context.Context, id string) error {
	marker := w.openAIMarker(id)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	name := "ws-" + strings.ToLower(id)
	for _, args := range [][]string{{"stop", name}, {"secret", "rm", "openai", "--sandbox", name, "--force"}} {
		cmd := command(ctx, w.Executable, args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if cmd.Run() != nil {
			return errors.New("Could not clear the previous sandbox API key; retry before starting another run")
		}
	}
	return os.Remove(marker)
}
func (w *Worker) recoverOpenAIRuns(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(w.Root, "openai-runs"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !identifier.MatchString(id) {
			return errors.New("Invalid API-key cleanup marker")
		}
		if err := w.recoverOpenAI(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
