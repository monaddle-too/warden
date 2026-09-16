package chats

import (
	"bytes"
	"testing"
)

func TestProposalFilePreservesReviewedBodyAndRejectsIdentity(t *testing.T) {
	p, err := decodeProposalFile([]byte(`{"repository":"owner/repo","base":"main","title":"Change","body":"Exact\nreviewed body","files":[{"path":"a.md","content":"full contents"}]}`))
	if err != nil || p["body"] != "Exact\nreviewed body" {
		t.Fatalf("body not preserved: %v", err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"chatID":"other"}`), []byte(`{"sandboxID":"other"}`),
		[]byte(`{"proposal_path":"recursive.json"}`), []byte(`null`), []byte(`[]`),
		[]byte(`{} {}`), {0xff}, bytes.Repeat([]byte(" "), (2<<20)+1),
	} {
		if _, err := decodeProposalFile(raw); err == nil {
			t.Fatal("accepted invalid proposal")
		}
	}
}
