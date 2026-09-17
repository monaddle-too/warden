package kube

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultServiceAccountDir is where kubelet projects a pod's service account
// token, CA bundle and namespace.
const DefaultServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// tokenRefreshInterval bounds how long a token read from TokenFile is used
// before the file is read again, so a rotated projected token is picked up.
const tokenRefreshInterval = time.Minute

// Config is what a Client needs to reach one API server.
type Config struct {
	// Host is the server URL, "https://10.43.0.1:443". A bare host:port is
	// taken as https.
	Host string
	// CAData is the PEM bundle that signs the server certificate; empty
	// means the system roots.
	CAData []byte
	// Insecure skips server certificate verification. Only a kubeconfig
	// with insecure-skip-tls-verify sets it.
	Insecure bool
	// CertData and KeyData are a PEM client certificate and key.
	CertData, KeyData []byte
	// Token is a static bearer token.
	Token string
	// TokenFile is a bearer token file that is re-read at most every
	// minute and after a 401, so rotated projected tokens are picked up.
	// It takes precedence over Token.
	TokenFile string
	// Namespace is the default namespace: the pod's own in cluster, the
	// context's in a kubeconfig; empty when neither says.
	Namespace string
}

// InClusterConfig reads the pod's own credentials: the API server address
// from KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT and the token,
// CA and namespace from DefaultServiceAccountDir.
func InClusterConfig() (*Config, error) {
	return InClusterConfigAt(DefaultServiceAccountDir)
}

// InClusterConfigAt is InClusterConfig with the service account directory
// given, for tests.
func InClusterConfigAt(dir string) (*Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset")
	}
	tokenFile := filepath.Join(dir, "token")
	if _, err := os.Stat(tokenFile); err != nil {
		return nil, fmt.Errorf("service account token: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	if _, err := certPool(ca); err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	cfg := &Config{Host: "https://" + joinHostPort(host, port), CAData: ca, TokenFile: tokenFile}
	if ns, err := os.ReadFile(filepath.Join(dir, "namespace")); err == nil {
		cfg.Namespace = strings.TrimSpace(string(ns))
	}
	return cfg, nil
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

// LoadKubeconfig reads a kubeconfig file (YAML or JSON) and returns the
// configuration of its current context: the cluster's server and CA (inline
// data or a file), the user's client certificate and key or bearer token,
// and the context's namespace. A user that authenticates through an exec
// plugin or an auth provider is refused with a clear error; this client has
// no plugin support. Relative file paths are resolved against the
// kubeconfig's directory.
func LoadKubeconfig(path string) (*Config, error) {
	return LoadKubeconfigContext(path, "")
}

// LoadKubeconfigContext is LoadKubeconfig for a named context; an empty name
// means the file's current-context (or its only context).
func LoadKubeconfigContext(path, context string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parseKubeconfig(data, filepath.Dir(path), context)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

func parseKubeconfig(data []byte, baseDir, contextName string) (*Config, error) {
	var doc any
	var err error
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		doc, err = parseJSONDocument(trimmed)
	} else {
		doc, err = parseYAML(data)
	}
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("not a kubeconfig document")
	}
	contexts := namedEntries(root["contexts"], "context")
	if contextName == "" {
		contextName, _ = root["current-context"].(string)
	}
	if contextName == "" {
		if len(contexts) != 1 {
			return nil, errors.New("no current-context")
		}
		for name := range contexts {
			contextName = name
		}
	}
	ctx, ok := contexts[contextName]
	if !ok {
		return nil, fmt.Errorf("context %q not found", contextName)
	}
	clusterName, _ := ctx["cluster"].(string)
	cluster, ok := namedEntries(root["clusters"], "cluster")[clusterName]
	if !ok {
		return nil, fmt.Errorf("cluster %q not found", clusterName)
	}
	cfg := &Config{}
	cfg.Host, _ = cluster["server"].(string)
	if cfg.Host == "" {
		return nil, fmt.Errorf("cluster %q has no server", clusterName)
	}
	cfg.Namespace, _ = ctx["namespace"].(string)
	cfg.Insecure = boolValue(cluster["insecure-skip-tls-verify"])
	if cfg.CAData, err = dataOrFile(cluster, "certificate-authority", baseDir); err != nil {
		return nil, err
	}
	if userName, _ := ctx["user"].(string); userName != "" {
		user, ok := namedEntries(root["users"], "user")[userName]
		if !ok {
			return nil, fmt.Errorf("user %q not found", userName)
		}
		if user["exec"] != nil {
			return nil, fmt.Errorf("user %q authenticates through an exec plugin, which this client does not support; use a certificate or a token", userName)
		}
		if user["auth-provider"] != nil {
			return nil, fmt.Errorf("user %q uses an auth provider, which this client does not support; use a certificate or a token", userName)
		}
		if cfg.CertData, err = dataOrFile(user, "client-certificate", baseDir); err != nil {
			return nil, err
		}
		if cfg.KeyData, err = dataOrFile(user, "client-key", baseDir); err != nil {
			return nil, err
		}
		if (cfg.CertData == nil) != (cfg.KeyData == nil) {
			return nil, fmt.Errorf("user %q has a client certificate without a key or a key without a certificate", userName)
		}
		cfg.Token, _ = user["token"].(string)
		if file, _ := user["tokenFile"].(string); file != "" {
			cfg.TokenFile = resolvePath(file, baseDir)
		}
	}
	if cfg.CertData == nil && cfg.Token == "" && cfg.TokenFile == "" {
		return nil, fmt.Errorf("context %q has no client certificate and no token", contextName)
	}
	return cfg, nil
}

// namedEntries turns a kubeconfig list of {name, <key>: {...}} into a map
// by name.
func namedEntries(list any, key string) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := list.([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		name, _ := entry["name"].(string)
		body, _ := entry[key].(map[string]any)
		if name != "" && body != nil {
			out[name] = body
		}
	}
	return out
}

// dataOrFile reads "<key>-data" (base64) or the file named by "<key>".
func dataOrFile(m map[string]any, key, baseDir string) ([]byte, error) {
	if s, _ := m[key+"-data"].(string); s != "" {
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, fmt.Errorf("%s-data is not base64: %w", key, err)
		}
		return data, nil
	}
	if file, _ := m[key].(string); file != "" {
		data, err := os.ReadFile(resolvePath(file, baseDir))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		return data, nil
	}
	return nil, nil
}

func resolvePath(p, baseDir string) string {
	if filepath.IsAbs(p) || baseDir == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

func boolValue(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	}
	return false
}

// tlsConfig builds the TLS configuration for the host.
func (c *Config) tlsConfig() (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.Insecure}
	if len(c.CAData) > 0 {
		pool, err := certPool(c.CAData)
		if err != nil {
			return nil, err
		}
		t.RootCAs = pool
	}
	if len(c.CertData) > 0 || len(c.KeyData) > 0 {
		cert, err := tls.X509KeyPair(c.CertData, c.KeyData)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}
	return t, nil
}

func certPool(pem []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates in CA bundle")
	}
	return pool, nil
}

// baseURL parses Host.
func (c *Config) baseURL() (*url.URL, error) {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return nil, errors.New("no API server host")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("API server host: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("API server host %q: scheme must be https or http", c.Host)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("API server host %q has no host", c.Host)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// tokenSource serves the bearer token, re-reading TokenFile at most every
// tokenRefreshInterval and on demand after a 401.
type tokenSource struct {
	static string
	file   string
	now    func() time.Time

	mu     sync.Mutex
	token  string
	loaded time.Time
}

func newTokenSource(cfg *Config) *tokenSource {
	return &tokenSource{static: cfg.Token, file: cfg.TokenFile, now: time.Now}
}

// refreshable reports whether a 401 can be answered by re-reading a file.
func (t *tokenSource) refreshable() bool { return t.file != "" }

// get returns the current token; force re-reads the file regardless of age.
func (t *tokenSource) get(force bool) (string, error) {
	if t.file == "" {
		return t.static, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !force && t.token != "" && now.Sub(t.loaded) < tokenRefreshInterval {
		return t.token, nil
	}
	data, err := os.ReadFile(t.file)
	if err != nil {
		if t.token != "" {
			return t.token, nil // keep serving the last good token
		}
		return "", fmt.Errorf("bearer token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		if t.token != "" {
			return t.token, nil
		}
		return "", errors.New("bearer token file is empty")
	}
	t.token, t.loaded = token, now
	return token, nil
}
