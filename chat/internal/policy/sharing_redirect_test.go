package policy

import "testing"

func TestGoogleRedirectFromBase(t *testing.T) {
	got, err := GoogleRedirectFromBase("http://127.0.0.1:28781/")
	if err != nil || got != "http://127.0.0.1:28781/oauth/google_docs/callback" {
		t.Fatalf("redirect %q %v", got, err)
	}
	for _, bad := range []string{"", "127.0.0.1:28781", "http://127.0.0.1:28781/app", "ftp://x"} {
		if _, err := GoogleRedirectFromBase(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	g, err := NewGoogleConnectionWithClient(t.TempDir(), GoogleClientOptions{RedirectBase: "http://127.0.0.1:28781", BuiltinClientID: "123-abc.apps.googleusercontent.com", BuiltinClientSecret: "GOCSPX-test-secret"}, nil)
	if err != nil || g.RedirectURI != "http://127.0.0.1:28781/oauth/google_docs/callback" {
		t.Fatalf("built-in client under a base: %v %+v", err, g)
	}
}
