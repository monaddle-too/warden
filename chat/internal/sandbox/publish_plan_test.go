package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishPlanRecreatesExactReviewedGitObjectsWithoutGuestAccess(t *testing.T) {
	w, runtime, req, git := reviewFixture(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(runtime.directory, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtime.directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("old-dir/file.md", "Replace this directory\n")
	write("old-file", "Replace this file\n")
	git("add", ".")
	git("commit", "-m", "Base tree transitions")
	s := w.managed.Sandboxes[req.SandboxID]
	s.Base = git("rev-parse", "HEAD")
	baseBundle := filepath.Join(w.Root, "repository-bundles", s.ID, "review-base.bundle")
	git("bundle", "create", baseBundle, "HEAD")
	if err := os.Remove(filepath.Join(runtime.directory, "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(runtime.directory, "old-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runtime.directory, "old-file")); err != nil {
		t.Fatal(err)
	}
	write("old-dir", "Now a regular file\n")
	write("old-file/example.md", "Now a directory\n")
	write("nested/example.mdx", "<Example />\n")
	write("duplicate.mdx", "<Example />\n")
	write("binary.bin", string([]byte{0, 255, 10}))
	write("empty.txt", "")
	write("run.sh", "#!/bin/sh\ntrue\n")
	if err := os.Chmod(filepath.Join(runtime.directory, "run.sh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/example.mdx", filepath.Join(runtime.directory, "example-link")); err != nil {
		t.Fatal(err)
	}
	req.Operation, req.CallID = "review.capture", "plan-review"
	captured, err := w.dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Operation, req.Expected = "review.publish-plan", captured.Review.Head
	// No guest is consulted once the immutable capture exists, even after newer
	// work and a stopped environment. The proof comes from retained Git objects.
	write("nested/example.mdx", "Changed after capture\n")
	s.State = "stopped"
	response, err := w.dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the private JSON wire representation, including raw JSON bodies.
	var wire Response
	if err := json.Unmarshal(jsonBytes(response), &wire); err != nil {
		t.Fatal(err)
	}
	plan := wire.PublishPlan
	if plan == nil || plan.Head != captured.Review.Head || plan.ReviewID != captured.Review.ID || runtime.exports != 1 {
		t.Fatal("plan lost captured provenance")
	}
	originalDigest := plan.SHA256
	plan.SHA256 = ""
	hash := sha256.Sum256(jsonBytes(plan))
	plan.SHA256 = originalDigest
	if hex.EncodeToString(hash[:]) != originalDigest {
		t.Fatal("plan digest does not bind exact requests")
	}
	again, err := w.dispatch(context.Background(), req)
	if err != nil || again.PublishPlan.SHA256 != originalDigest {
		t.Fatal("plan changed on retry", err)
	}

	// Replay the GitHub REST object semantics into an independent bare repository
	// seeded only with the public base. Every requested object must hash exactly.
	dir := t.TempDir()
	run := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
		cmd.Env = publicGitEnvironment()
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "--bare", dir)
	run("", "fetch", baseBundle, s.Base+":refs/heads/base")
	blobCount := map[string]int{}
	for _, request := range plan.Requests {
		if request.Method != "POST" || !strings.HasPrefix(request.Path, "/repos/Canton-Network/cf-docs/git/") {
			t.Fatal("unexpected publication target", request)
		}
		hash := sha256.Sum256(request.Body)
		if request.BodySHA256 != hex.EncodeToString(hash[:]) {
			t.Fatal("request body digest mismatch")
		}
		var actual string
		switch strings.TrimPrefix(request.Path, "/repos/Canton-Network/cf-docs/git/") {
		case "blobs":
			var body struct {
				Content  []byte
				Encoding string
			}
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Encoding != "base64" || strings.Contains(string(request.Body), `"content":null`) {
				t.Fatal("invalid binary or empty blob encoding")
			}
			actual = run(string(body.Content), "hash-object", "-w", "--stdin")
			blobCount[actual]++
		case "trees":
			var body struct{ Tree []publicationTreeEntry }
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			var input strings.Builder
			for _, entry := range body.Tree {
				input.WriteString(entry.Mode + " " + entry.Type + " " + entry.SHA + "\t" + entry.Path + "\x00")
			}
			actual = run(input.String(), "mktree", "-z")
		case "commits":
			var body struct {
				Tree, Message     string
				Parents           []string
				Author, Committer struct{ Name, Email, Date string }
			}
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Author.Date != "2001-01-01T00:00:00Z" || body.Committer != body.Author || len(body.Parents) != 1 {
				t.Fatal("unexpected commit identity")
			}
			input := "tree " + body.Tree + "\nparent " + body.Parents[0] + "\nauthor " + body.Author.Name + " <" + body.Author.Email + "> 978307200 +0000\ncommitter " + body.Committer.Name + " <" + body.Committer.Email + "> 978307200 +0000\n\n" + body.Message
			actual = run(input, "hash-object", "-t", "commit", "-w", "--stdin")
		default:
			t.Fatal("plan contains branch/PR authority before human approval")
		}
		if actual != request.ExpectedSHA {
			t.Fatal("REST object would differ from reviewed object", request.Path, actual, request.ExpectedSHA)
		}
	}
	for _, count := range blobCount {
		if count != 1 {
			t.Fatal("duplicate blob request")
		}
	}
	if got := run("", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", plan.Base, plan.Head, "--"); got != strings.TrimSpace(captured.Review.Diff) {
		t.Fatal("published tree differs from the reviewed diff")
	}
	if len(plan.Requests) == 0 || plan.Requests[len(plan.Requests)-1].ExpectedSHA != plan.Head {
		t.Fatal("plan does not finish with reviewed commit")
	}
}

func TestPublishPlanRejectsTamperedBundleMetadataAndSelectors(t *testing.T) {
	w, _, req, _ := reviewFixture(t)
	req.Operation, req.CallID = "review.capture", "tamper-review"
	// Ensure there is a change, otherwise publication correctly refuses an empty review.
	s := w.managed.Sandboxes[req.SandboxID]
	if err := os.WriteFile(filepath.Join(s.Directory, "plan.md"), []byte("Reviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := w.dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Operation, req.Expected = "review.publish-plan", captured.Review.Head
	foreign := req
	foreign.PrincipalID = "mallory"
	if _, err = w.dispatch(context.Background(), foreign); err == nil {
		t.Fatal("foreign principal accepted")
	}
	changed := req
	changed.Expected = strings.Repeat("1", 40)
	if _, err = w.dispatch(context.Background(), changed); err == nil {
		t.Fatal("unreviewed head accepted")
	}
	root := filepath.Join(w.reviewRoot(req.ChatID), req.CallID)
	metadata := *captured.Review
	metadata.Diff = "forged review"
	if err = atomicJSON(filepath.Join(root, "review.json"), metadata); err != nil {
		t.Fatal(err)
	}
	if _, err = w.dispatch(context.Background(), req); err == nil {
		t.Fatal("metadata mismatch accepted")
	}
	if err = atomicJSON(filepath.Join(root, "review.json"), captured.Review); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "snapshot.bundle"), []byte("corrupt transfer"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = w.dispatch(context.Background(), req); err == nil {
		t.Fatal("bundle digest mismatch accepted")
	}
}
