package policy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DocumentMaxBody     = 2 * 1024 * 1024
	DocumentMaxResponse = 4 * 1024 * 1024
	DocumentPrefix      = "/workspace/v1/documents"
	documentUpstream    = "/agent/v1/documents"
	documentIDPattern   = `[A-Za-z0-9][A-Za-z0-9_-]{0,127}`
)

var documentReadRoute = regexp.MustCompile(`^` + regexp.QuoteMeta(DocumentPrefix) + `/` + documentIDPattern + `(?:/(?:revisions|proposals|comments/replies))?$`)
var documentWriteRoute = regexp.MustCompile(`^` + regexp.QuoteMeta(DocumentPrefix) + `/` + documentIDPattern + `(?:/proposals|/comments/` + documentIDPattern + `/replies)$`)
var documentOffset = regexp.MustCompile(`^[0-9]{1,9}$`)

// DocumentPath validates a guest document route and returns the upstream path.
func DocumentPath(method, path, projectID string) (string, error) {
	if len(path) > 512 {
		return "", errors.New("invalid document route")
	}
	query := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i+1:]
		names := map[string]bool{}
		for _, field := range strings.Split(query, "&") {
			key, value, ok := strings.Cut(field, "=")
			if !ok || names[key] {
				return "", errors.New("invalid document query")
			}
			names[key] = true
			if key == "projectID" && path == DocumentPrefix && value == projectID {
				continue
			}
			if key == "offset" && method == "GET" && documentOffset.MatchString(value) && (path == DocumentPrefix || strings.HasSuffix(path, "/revisions") || strings.HasSuffix(path, "/proposals") || strings.HasSuffix(path, "/comments/replies")) {
				continue
			}
			return "", errors.New("invalid document query")
		}
		query = "?" + query
	}
	valid := method == "GET" && (path == DocumentPrefix || documentReadRoute.MatchString(path))
	valid = valid || (method == "POST" && documentWriteRoute.MatchString(path))
	if !valid {
		return "", errors.New("unsupported document route")
	}
	return documentUpstream + strings.TrimPrefix(path, DocumentPrefix) + query, nil
}

// DocumentAPI dispatches signed document requests to a fixed loopback app.
// Signing keys never enter the gateway or a guest.
type DocumentAPI struct {
	host      string
	authority string
	port      int
	Clock     Clock
	key       []byte
	// Dispatch is replaceable in tests.
	Dispatch func(method, path string, body []byte, headers map[string]string) (int, []byte, error)
}

func NewDocumentAPI(origin, keyFile string) (*DocumentAPI, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, errors.New("document API origin must be a canonical HTTP loopback origin with port")
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if u.Scheme != "http" || (host != "localhost" && host != "127.0.0.1" && host != "::1") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || origin != "http://"+u.Host || u.Port() == "" || port < 1 || port > 65535 {
		return nil, errors.New("document API origin must be a canonical HTTP loopback origin with port")
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	raw, err := openPrivateStrict(keyFile, 4096)
	if err != nil {
		return nil, err
	}
	key := bytes.TrimRight(raw, " \t\r\n")
	if len(key) < 32 || len(key) > 4096 {
		return nil, errors.New("document API key must contain at least 32 printable ASCII characters")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return nil, errors.New("document API key must contain at least 32 printable ASCII characters")
		}
	}
	api := &DocumentAPI{host: host, authority: u.Host, port: port, key: key}
	api.Dispatch = api.dispatch
	return api, nil
}

// openPrivateStrict returns an error for symlinks (OSError in Python) and a
// distinct error for public or oversized files.
func openPrivateStrict(path string, limit int64) ([]byte, error) {
	raw, err := openPrivate(path, limit, "document API key")
	if err != nil {
		if strings.Contains(err.Error(), "must be private") {
			return nil, errors.New("document API key must be a private owned regular file")
		}
		return nil, err
	}
	return raw, nil
}

func (d *DocumentAPI) now() float64 {
	if d.Clock != nil {
		return d.Clock()
	}
	return wallClock()
}

// Prepare validates a request and produces the signed upstream headers.
func (d *DocumentAPI) Prepare(method, path string, body []byte, ctx map[string]any) (string, map[string]string, error) {
	projectID, _ := ctx["projectID"].(string)
	path, err := DocumentPath(method, path, projectID)
	if err != nil {
		return "", nil, err
	}
	if len(body) > DocumentMaxBody {
		return "", nil, errors.New("document body too large")
	}
	if method == "GET" && len(body) > 0 {
		return "", nil, errors.New("document reads must not have a body")
	}
	if method == "POST" {
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
			return "", nil, errors.New("document mutation must be a JSON object")
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(Dumps(ctx)))
	timestamp := strconv.FormatInt(int64(d.now()), 10)
	nonce := randomHex(32)
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{method, path, hex.EncodeToString(digest[:]), timestamp, nonce, encoded}, "\n")
	mac := hmac.New(sha256.New, d.key)
	mac.Write([]byte(canonical))
	headers := map[string]string{"Host": d.authority, "Content-Type": "application/json", "Accept": "application/json",
		"Accept-Encoding": "identity", "X-Warden-Context": encoded, "X-Warden-Timestamp": timestamp,
		"X-Warden-Nonce": nonce, "X-Warden-Signature": hex.EncodeToString(mac.Sum(nil))}
	return path, headers, nil
}

var documentStatuses = map[int]bool{400: true, 401: true, 403: true, 404: true, 409: true, 413: true, 422: true, 429: true}

// dispatch performs the bounded loopback HTTP exchange without proxies or
// redirects. The whole exchange, including a trickling body, is limited to
// five seconds.
func (d *DocumentAPI) dispatch(method, path string, body []byte, headers map[string]string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(d.host, strconv.Itoa(d.port))+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	if !(res.StatusCode >= 200 && res.StatusCode < 300) && !documentStatuses[res.StatusCode] {
		return 0, nil, errors.New("document upstream returned unsupported status")
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(res.Header.Get("Content-Type"), ";", 2)[0]))
	encoding := strings.ToLower(res.Header.Get("Content-Encoding"))
	if contentType != "application/json" || (encoding != "" && encoding != "identity") {
		return 0, nil, errors.New("document upstream must return uncompressed JSON")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, DocumentMaxResponse+1))
	if err != nil {
		return 0, nil, err
	}
	if len(data) > DocumentMaxResponse {
		return 0, nil, errors.New("document response too large")
	}
	var parsed any
	if err = json.Unmarshal(data, &parsed); err != nil {
		return 0, nil, err
	}
	switch parsed.(type) {
	case map[string]any, []any:
	default:
		return 0, nil, errors.New("document response must be a JSON object or array")
	}
	// Never send the key, request signature or trusted context back to a
	// guest even if a misconfigured local upstream reflects its input.
	rendered, _ := json.Marshal(parsed)
	inspected := append(append(rendered, '\n'), data...)
	for _, protected := range [][]byte{d.key, []byte(headers["X-Warden-Signature"]), []byte(headers["X-Warden-Context"])} {
		if len(protected) > 0 && bytes.Contains(inspected, protected) {
			return 0, nil, errors.New("document response exposed protected authentication")
		}
	}
	return res.StatusCode, data, nil
}
