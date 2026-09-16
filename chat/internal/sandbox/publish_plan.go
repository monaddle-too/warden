package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const maxPublicationRequest = 8 << 20
const maxPublicationPlan = 64 << 20

// Each request has an independently verifiable expected object hash. Neither
// this plan nor its digest authorizes any GitHub request on its own.
type RepositoryMutation struct {
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Body        json.RawMessage `json:"body"`
	BodySHA256  string          `json:"bodySHA256"`
	ExpectedSHA string          `json:"expectedSHA"`
}
type RepositoryPublishPlan struct {
	ReviewID     string               `json:"reviewID"`
	ProjectID    string               `json:"projectID"`
	ChatID       string               `json:"chatID"`
	SandboxID    string               `json:"sandboxID"`
	Repository   string               `json:"repository"`
	Base         string               `json:"base"`
	Head         string               `json:"head"`
	BundleSHA256 string               `json:"bundleSHA256"`
	Requests     []RepositoryMutation `json:"requests"`
	SHA256       string               `json:"sha256"`
}

func (w *Worker) publishPlanLocked(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	review, err := w.readReviewLocked(s, r, r.CallID)
	if err != nil {
		return Response{}, err
	}
	if r.Repository != review.Repository || r.Expected != review.Head {
		return Response{}, errors.New("publication must name the exact reviewed repository and head")
	}
	if _, err := publicRepositoryURL(review.Repository); err != nil || !commit.MatchString(review.Head) {
		return Response{}, errors.New("invalid retained publication identity")
	}
	snapshot := *s
	// Only immutable retained data is read. The environment may be stopped or
	// doing newer work; status and cancellation stay available during verification.
	w.mu.Unlock()
	defer w.mu.Lock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	root := filepath.Join(w.reviewRoot(r.ChatID), review.ID)
	file, err := os.Open(filepath.Join(root, "snapshot.bundle"))
	if err != nil {
		return Response{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxReviewBundle+1))
	if err != nil {
		return Response{}, err
	}
	digest := sha256.Sum256(raw)
	if len(raw) > maxReviewBundle || hex.EncodeToString(digest[:]) != review.BundleSHA256 {
		return Response{}, errors.New("retained review bundle does not match its recorded digest")
	}
	stage, err := os.MkdirTemp(w.Root, ".publish-plan-")
	if err != nil {
		return Response{}, err
	}
	defer os.RemoveAll(stage)
	// Verify the exact bytes whose digest was checked, rather than reopening the
	// original path during Git import.
	bundle := filepath.Join(stage, "snapshot.bundle")
	if err = os.WriteFile(bundle, raw, 0600); err != nil {
		return Response{}, err
	}
	baseBundle, err := w.reviewBaseBundle(ctx, snapshot)
	if err != nil {
		return Response{}, err
	}
	plan := RepositoryPublishPlan{ReviewID: review.ID, ProjectID: review.ProjectID, ChatID: review.ChatID, SandboxID: review.SandboxID, Repository: review.Repository, Base: review.Base, Head: review.Head, BundleSHA256: review.BundleSHA256, Requests: []RepositoryMutation{}}
	verified, err := verifyReviewBundle(ctx, stage, bundle, review.Base, baseBundle, func(dir string) error {
		requests, err := publicationObjects(ctx, dir, review)
		plan.Requests = requests
		return err
	})
	if err != nil {
		return Response{}, err
	}
	if verified.Head != review.Head || verified.Diff != review.Diff || !bytes.Equal(jsonBytes(verified.Files), jsonBytes(review.Files)) {
		return Response{}, errors.New("retained review metadata does not match its verified objects")
	}
	if len(review.Files) == 0 {
		return Response{}, errors.New("there are no reviewed changes to publish")
	}
	encoded := jsonBytes(plan)
	if len(encoded) > maxPublicationPlan {
		return Response{}, errors.New("publication plan exceeds 64 MiB")
	}
	hash := sha256.Sum256(encoded)
	plan.SHA256 = hex.EncodeToString(hash[:])
	return Response{PublishPlan: &plan}, nil
}
func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }

type publicationTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func publicationObjects(ctx context.Context, dir string, review *RepositoryReview) ([]RepositoryMutation, error) {
	git := func(limit int, args ...string) ([]byte, error) {
		cmd := command(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "--git-dir=" + dir}, args...)...)
		cmd.Env = publicGitEnvironment()
		var out bytes.Buffer
		cmd.Stdout = &limitedWriter{W: &out, N: limit}
		if err := cmd.Run(); err != nil {
			return nil, errors.New("could not read verified publication objects or object exceeds publication limit")
		}
		return out.Bytes(), nil
	}
	readTree := func(hash string) ([]publicationTreeEntry, error) {
		if hash == "" {
			return nil, nil
		}
		raw, err := git(maxPublicationRequest, "ls-tree", "-z", hash)
		if err != nil {
			return nil, err
		}
		entries := []publicationTreeEntry{}
		for _, row := range bytes.Split(raw, []byte{0}) {
			if len(row) == 0 {
				continue
			}
			parts := bytes.SplitN(row, []byte{'\t'}, 2)
			if len(parts) != 2 || !utf8.Valid(parts[1]) || bytes.ContainsAny(parts[1], "/\\") {
				return nil, errors.New("unsupported publication tree name")
			}
			fields := strings.Fields(string(parts[0]))
			if len(fields) != 3 || !commit.MatchString(fields[2]) {
				return nil, errors.New("invalid publication tree entry")
			}
			if !(fields[0] == "040000" && fields[1] == "tree" || fields[0] == "160000" && fields[1] == "commit" || (fields[0] == "100644" || fields[0] == "100755" || fields[0] == "120000") && fields[1] == "blob") {
				return nil, errors.New("unsupported publication tree mode")
			}
			entries = append(entries, publicationTreeEntry{Path: string(parts[1]), Mode: fields[0], Type: fields[1], SHA: fields[2]})
		}
		return entries, nil
	}
	baseRaw, err := git(256, "rev-parse", review.Base+"^{tree}")
	if err != nil {
		return nil, err
	}
	headRaw, err := git(256, "rev-parse", review.Head+"^{tree}")
	if err != nil {
		return nil, err
	}
	baseTree, headTree := strings.TrimSpace(string(baseRaw)), strings.TrimSpace(string(headRaw))
	prefix := "/repos/" + strings.TrimPrefix(review.Repository, "github://") + "/git/"
	requests := []RepositoryMutation{}
	total := 0
	appendRequest := func(resource, hash string, body any) error {
		raw := jsonBytes(body)
		total += len(raw)
		if len(raw) > maxPublicationRequest || total > maxPublicationPlan || len(requests) >= 8192 {
			return errors.New("publication object requests exceed supported limits")
		}
		digest := sha256.Sum256(raw)
		requests = append(requests, RepositoryMutation{Method: "POST", Path: prefix + resource, Body: raw, BodySHA256: hex.EncodeToString(digest[:]), ExpectedSHA: hash})
		return nil
	}
	seen := map[string]bool{}
	var walk func(string, string, int) error
	walk = func(base, head string, depth int) error {
		if head == base || seen[head] {
			return nil
		}
		if depth > 128 {
			return errors.New("publication tree exceeds maximum nesting")
		}
		previous, err := readTree(base)
		if err != nil {
			return err
		}
		entries, err := readTree(head)
		if err != nil {
			return err
		}
		old := map[string]publicationTreeEntry{}
		for _, entry := range previous {
			old[entry.Path] = entry
		}
		for _, entry := range entries {
			existing := old[entry.Path]
			if existing.SHA == entry.SHA && existing.Type == entry.Type || seen[entry.SHA] {
				continue
			}
			switch entry.Type {
			case "tree":
				baseChild := ""
				if existing.Type == "tree" {
					baseChild = existing.SHA
				}
				if err := walk(baseChild, entry.SHA, depth+1); err != nil {
					return err
				}
			case "blob":
				content, err := git(6<<20, "cat-file", "blob", entry.SHA)
				if err != nil {
					return err
				}
				if content == nil {
					content = []byte{}
				}
				if err := appendRequest("blobs", entry.SHA, struct {
					Content  []byte `json:"content"`
					Encoding string `json:"encoding"`
				}{content, "base64"}); err != nil {
					return err
				}
				seen[entry.SHA] = true
			}
		}
		if err := appendRequest("trees", head, struct {
			Tree []publicationTreeEntry `json:"tree"`
		}{entries}); err != nil {
			return err
		}
		seen[head] = true
		return nil
	}
	if err = walk(baseTree, headTree, 0); err != nil {
		return nil, err
	}
	rawCommit, err := git(1<<20, "cat-file", "commit", review.Head)
	if err != nil {
		return nil, err
	}
	expectedCommit := fmt.Sprintf("tree %s\nparent %s\nauthor Panta <panta@localhost> 978307200 +0000\ncommitter Panta <panta@localhost> 978307200 +0000\n\nPanta reviewed snapshot\n", headTree, review.Base)
	if string(rawCommit) != expectedCommit {
		return nil, errors.New("snapshot commit metadata is not supported for exact publication")
	}
	identity := map[string]string{"name": "Panta", "email": "panta@localhost", "date": "2001-01-01T00:00:00Z"}
	if err = appendRequest("commits", review.Head, map[string]any{"message": "Panta reviewed snapshot\n", "tree": headTree, "parents": []string{review.Base}, "author": identity, "committer": identity}); err != nil {
		return nil, err
	}
	return requests, nil
}
