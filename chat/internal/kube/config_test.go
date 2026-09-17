package kube

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// k3sKubeconfig is the shape /etc/rancher/k3s/k3s.yaml has.
const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: default
  user:
    client-certificate-data: %s
    client-key-data: %s
`

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLoadKubeconfigK3s(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k3s.yaml")
	content := fmt.Sprintf(k3sKubeconfig, b64("CA PEM"), b64("CERT PEM"), b64("KEY PEM"))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadKubeconfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{Host: "https://127.0.0.1:6443", CAData: []byte("CA PEM"), CertData: []byte("CERT PEM"), KeyData: []byte("KEY PEM")}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadKubeconfigFilesTokenAndContexts(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"ca.crt": "CA FILE", "client.crt": "CERT FILE", "client.key": "KEY FILE", "token.txt": "file-token\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config")
	content := `# written by the dev loop
apiVersion: v1
kind: Config
current-context: dev   # the VM
clusters:
  - name: dev
    cluster:
      server: "https://dev.example:6443"   # quoted
      certificate-authority: ca.crt
  - name: insecure
    cluster:
      server: https://10.0.0.5:6443
      insecure-skip-tls-verify: true
contexts:
  - name: dev
    context:
      cluster: dev
      user: certs
      namespace: warden
  - name: token
    context: {cluster: insecure, user: token}
  - name: exec
    context:
      cluster: dev
      user: plugin
  - name: file-token
    context:
      cluster: dev
      user: file-token
users:
  - name: certs
    user:
      client-certificate: client.crt
      client-key: client.key
  - name: token
    user:
      token: 'it''s a token'
  - name: plugin
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: aws
        args:
          - eks
          - get-token
  - name: file-token
    user:
      tokenFile: token.txt
`
	// The token context uses a flow mapping, which the parser refuses; keep
	// it block style but check the error path first.
	if _, err := parseKubeconfig([]byte(content), dir, "token"); err == nil || !strings.Contains(err.Error(), "flow collections") {
		t.Fatalf("flow mapping accepted: %v", err)
	}
	content = strings.Replace(content, "    context: {cluster: insecure, user: token}\n", "    context:\n      cluster: insecure\n      user: token\n", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadKubeconfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{Host: "https://dev.example:6443", CAData: []byte("CA FILE"), CertData: []byte("CERT FILE"), KeyData: []byte("KEY FILE"), Namespace: "warden"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	cfg, err = LoadKubeconfigContext(path, "token")
	if err != nil {
		t.Fatal(err)
	}
	want = &Config{Host: "https://10.0.0.5:6443", Insecure: true, Token: "it's a token"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	cfg, err = LoadKubeconfigContext(path, "file-token")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenFile != filepath.Join(dir, "token.txt") || cfg.Token != "" {
		t.Fatalf("got %+v", cfg)
	}
	if _, err = LoadKubeconfigContext(path, "exec"); err == nil || !strings.Contains(err.Error(), "exec plugin") {
		t.Fatalf("exec plugin not refused: %v", err)
	}
	if _, err = LoadKubeconfigContext(path, "nope"); err == nil || !strings.Contains(err.Error(), `context "nope" not found`) {
		t.Fatalf("missing context: %v", err)
	}
	if _, err = LoadKubeconfig(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLoadKubeconfigJSON(t *testing.T) {
	doc := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": "j",
		"clusters":        []any{map[string]any{"name": "c", "cluster": map[string]any{"server": "https://j:6443", "certificate-authority-data": b64("CA"), "insecure-skip-tls-verify": false}}},
		"contexts":        []any{map[string]any{"name": "j", "context": map[string]any{"cluster": "c", "user": "u", "namespace": "n"}}},
		"users":           []any{map[string]any{"name": "u", "user": map[string]any{"token": "t"}}},
	}
	raw, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadKubeconfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{Host: "https://j:6443", CAData: []byte("CA"), Token: "t", Namespace: "n"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	// A single context needs no current-context.
	delete(doc, "current-context")
	raw, _ = json.Marshal(doc)
	if _, err := parseKubeconfig(raw, "", ""); err != nil {
		t.Fatal(err)
	}
	// Errors are clear.
	cases := map[string]string{
		`{"contexts":[{"name":"a","context":{}},{"name":"b","context":{}}]}`:                                                                                                                                                              "no current-context",
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c"}}],"clusters":[]}`:                                                                                                                                       `cluster "c" not found`,
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c","user":"u"}}],"clusters":[{"name":"c","cluster":{"server":"https://x"}}]}`:                                                                               `user "u" not found`,
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c"}}],"clusters":[{"name":"c","cluster":{"server":"https://x"}}]}`:                                                                                          "no client certificate and no token",
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c"}}],"clusters":[{"name":"c","cluster":{}}]}`:                                                                                                              "has no server",
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c","user":"u"}}],"clusters":[{"name":"c","cluster":{"server":"https://x"}}],"users":[{"name":"u","user":{"client-certificate-data":"QUJD"}}]}`:              "without a key",
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c","user":"u"}}],"clusters":[{"name":"c","cluster":{"server":"https://x"}}],"users":[{"name":"u","user":{"auth-provider":{"name":"gcp"}}}]}`:                "auth provider",
		`{"current-context":"a","contexts":[{"name":"a","context":{"cluster":"c","user":"u"}}],"clusters":[{"name":"c","cluster":{"server":"https://x","certificate-authority-data":"!!"}}],"users":[{"name":"u","user":{"token":"t"}}]}`: "not base64",
		`[1]`:    "not a kubeconfig",
		`{"x": `: "invalid JSON",
	}
	for input, want := range cases {
		_, err := parseKubeconfig([]byte(input), "", "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", input, err, want)
		}
	}
}

func TestInClusterConfig(t *testing.T) {
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: newFakeAPI(t).srv.Certificate().Raw})
	for name, content := range map[string]string{"token": "sa-token", "ca.crt": string(ca), "namespace": "warden\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := InClusterConfigAt(dir); err == nil || !strings.Contains(err.Error(), "not running in a cluster") {
		t.Fatalf("without the environment: %v", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	cfg, err := InClusterConfigAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://10.43.0.1:443" || string(cfg.CAData) != string(ca) || cfg.TokenFile != filepath.Join(dir, "token") || cfg.Token != "" || cfg.Namespace != "warden" {
		t.Fatalf("got %+v", cfg)
	}
	// The token is read through the file, so a rotation is seen.
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if token, _ := c.token.get(false); token != "sa-token" {
		t.Fatalf("token %q", token)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, _ := c.token.get(true); token != "rotated" {
		t.Fatalf("token %q", token)
	}
	// An unreadable file keeps the last good token; a first read failure is an error.
	if err := os.Remove(filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if token, err := c.token.get(true); err != nil || token != "rotated" {
		t.Fatalf("token %q %v", token, err)
	}
	if _, err := InClusterConfigAt(dir); err == nil {
		t.Fatal("missing token accepted")
	}
	fresh := newTokenSource(&Config{TokenFile: filepath.Join(dir, "token")})
	if _, err := fresh.get(false); err == nil {
		t.Fatal("missing token file accepted")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := InClusterConfigAt(dir); err != nil || cfg.Host != "https://[fd00::1]:443" {
		t.Fatalf("IPv6 host: %+v %v", cfg, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InClusterConfigAt(dir); err == nil {
		t.Fatal("bad CA accepted")
	}
}
