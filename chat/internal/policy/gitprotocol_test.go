package policy

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// git gzips any RPC body over 1 KiB, so a full clone of a repository with
// a few dozen refs arrives compressed: the body is inflated for
// inspection and forwarding, bounded by the limit, and the header goes.
func TestGitDecodeBodyInflatesGzip(t *testing.T) {
	want := []byte(strings.Repeat("0032want 0123456789abcdef0123456789abcdef01234567\n", 40) + "0000")
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(want)
	_ = w.Close()
	headers := [][]string{{"Content-Type", "application/x-git-upload-pack-request"}, {"Content-Encoding", "gzip"}, {"Git-Protocol", "version=2"}}
	got, body, err := GitDecodeBody(headers, buf.Bytes(), requestInspectionLimit)
	if err != nil || !bytes.Equal(body, want) || len(got) != 2 || got[1][0] != "Git-Protocol" {
		t.Fatalf("inflate: %v %d %v", err, len(body), got)
	}
	if _, err := GitInspect("POST", "/o/r.git/git-upload-pack", got, body); err != nil {
		t.Fatalf("inspect inflated: %v", err)
	}
	// Identity bodies pass through untouched; other encodings stay refused
	// by GitInspect; a body that inflates past the limit is refused; junk
	// under a gzip header is refused.
	if h, b, err := GitDecodeBody(headers[:1], want, requestInspectionLimit); err != nil || !bytes.Equal(b, want) || len(h) != 1 {
		t.Fatalf("identity: %v", err)
	}
	br := [][]string{{"Content-Type", "application/x-git-upload-pack-request"}, {"Content-Encoding", "br"}}
	if _, _, err := GitDecodeBody(br, want, requestInspectionLimit); err != nil {
		t.Fatalf("br left to inspect: %v", err)
	}
	if _, err := GitInspect("POST", "/o/r.git/git-upload-pack", br, want); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("br accepted: %v", err)
	}
	if _, _, err := GitDecodeBody(headers, buf.Bytes(), 100); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over limit: %v", err)
	}
	if _, _, err := GitDecodeBody(headers, []byte("not gzip"), requestInspectionLimit); err == nil {
		t.Fatal("junk accepted")
	}
}
