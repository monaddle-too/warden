package chats

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// Capture bytes once, before the broker creates the immutable review snapshot.
// The worker uses descriptor-relative reads and rejects symlinks/nonregular files.
func (e *Engine) proposalInput(ctx context.Context, c *Chat, input map[string]any) (map[string]any, error) {
	v, exists := input["proposal_path"]
	if !exists {
		return input, nil
	}
	path, ok := v.(string)
	if !ok || path == "" || len(path) > 1024 || len(input) != 1 {
		return nil, errors.New("provide only proposal_path with a relative workspace JSON file")
	}
	r := request(c, "proposal-file")
	r.Directory = path
	result, err := e.Worker.Call(ctx, r)
	if err != nil {
		return nil, err
	}
	return decodeProposalFile(result.Bytes)
}

func decodeProposalFile(raw []byte) (map[string]any, error) {
	var proposal map[string]any
	if len(raw) > 2<<20 || !utf8.Valid(raw) || json.Unmarshal(raw, &proposal) != nil || proposal == nil {
		return nil, errors.New("proposal file must be a UTF-8 JSON object up to 2 MiB")
	}
	for key := range proposal {
		switch key {
		case "repository", "base", "title", "body", "files", "images":
		default:
			return nil, errors.New("unexpected proposal field")
		}
	}
	return proposal, nil
}
