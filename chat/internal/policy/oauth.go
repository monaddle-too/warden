package policy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Exchange performs one fixed OAuth token request. Tests substitute it.
type Exchange func(clientID, clientSecret, endpoint string, fields map[string]string) (map[string]any, error)

// oauthProvider describes the provider-specific parts of a connection.
type oauthProvider struct {
	name            string
	scopes          []string
	clientIDPattern *regexp.Regexp
	callbackPath    string
	authorizeURL    string
	tokenEndpoint   string
	refreshEndpoint string
	offlineParams   bool
	requireScopes   bool // token response scope must equal the requested set
	subsetScopes    bool // token response scope, when present, must be a subset
	tokenHost       string
	basicAuth       bool
}

var googleDocsProvider = oauthProvider{
	name:            "Google Docs",
	scopes:          []string{"https://www.googleapis.com/auth/documents.readonly"},
	clientIDPattern: regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}\.apps\.googleusercontent\.com$`),
	callbackPath:    "/oauth/google_docs/callback",
	authorizeURL:    "https://accounts.google.com/o/oauth2/v2/auth?",
	tokenEndpoint:   "/token",
	refreshEndpoint: "/token",
	offlineParams:   true,
	requireScopes:   true,
	tokenHost:       "oauth2.googleapis.com",
}

var figmaProvider = oauthProvider{
	name:            "Figma",
	scopes:          []string{"current_user:read", "file_content:read", "file_metadata:read", "file_comments:read"},
	clientIDPattern: regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`),
	callbackPath:    "/oauth/figma/callback",
	authorizeURL:    "https://www.figma.com/oauth?",
	tokenEndpoint:   "/v1/oauth/token",
	refreshEndpoint: "/v1/oauth/refresh",
	subsetScopes:    true,
	tokenHost:       "api.figma.com",
	basicAuth:       true,
}

func printable(c byte) bool { return c >= '!' && c <= '~' }

func clientSecretShape(s string) bool { return boundedClass(s, 8, 4096, printable) }
func authCodeShape(s string) bool     { return boundedClass(s, 1, 4096, printable) }
func oauthTokenShape(s string) bool {
	return boundedClass(s, 16, 8192, func(c byte) bool { return isAlnum(c) || strings.IndexByte("._~+/=-", c) >= 0 })
}

var publicHostShape = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

// OAuthConnection holds a host-only PKCE connection. Credentials and pending
// state live only in this process unless a subclass persists them.
type OAuthConnection struct {
	provider  oauthProvider
	Redactor  *Redactor
	Clock     Clock
	Transport Exchange

	ClientID, ClientSecret, RedirectURI string
	pendingState, pendingVerifier       string
	pendingExpires                      float64
	AccessToken, RefreshToken           string
	Expires                             float64
	UserID                              string
}

func newOAuthConnection(provider oauthProvider, redactor *Redactor, clock Clock) *OAuthConnection {
	c := &OAuthConnection{provider: provider, Redactor: redactor, Clock: clock}
	c.Transport = c.exchange
	return c
}

// NewGoogleDocsConnection returns the read-only Google Docs adapter.
func NewGoogleDocsConnection(redactor *Redactor, clock Clock) *OAuthConnection {
	return newOAuthConnection(googleDocsProvider, redactor, clock)
}

// NewFigmaConnection returns the Figma adapter.
func NewFigmaConnection(redactor *Redactor, clock Clock) *OAuthConnection {
	return newOAuthConnection(figmaProvider, redactor, clock)
}

func (c *OAuthConnection) now() float64 {
	if c.Clock != nil {
		return c.Clock()
	}
	return wallClock()
}

// Configure installs the OAuth client; allowHTTPS admits a public callback.
func (c *OAuthConnection) Configure(clientID, clientSecret, redirectURI string, allowHTTPS bool) error {
	if !c.provider.clientIDPattern.MatchString(clientID) {
		return errors.New("invalid " + c.provider.name + " client ID")
	}
	if !clientSecretShape(clientSecret) {
		return errors.New("invalid " + c.provider.name + " client secret")
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return errors.New(c.provider.name + " callback must be the local control-plane URL")
	}
	local := u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Port() != "" && u.Host == "127.0.0.1:"+u.Port() && u.User == nil
	public := allowHTTPS && u.Scheme == "https" && u.Hostname() != "" && publicHostShape.MatchString(u.Hostname()) && u.Host == u.Hostname() && u.User == nil
	if !(local || public) || u.Path != c.provider.callbackPath || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return errors.New(c.provider.name + " callback must be the local control-plane URL")
	}
	c.Disconnect()
	c.ClientID, c.ClientSecret, c.RedirectURI = clientID, clientSecret, redirectURI
	c.Redactor.Register(clientSecret)
	return nil
}

func tokenURLSafe(n int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(n))
}

// Start begins a PKCE authorization and returns the provider URL.
func (c *OAuthConnection) Start() (string, error) {
	return c.startWithScopes(c.provider.scopes)
}

func (c *OAuthConnection) startWithScopes(scopes []string) (string, error) {
	if c.ClientID == "" {
		return "", errors.New("configure a " + c.provider.name + " OAuth app first")
	}
	state, verifier := tokenURLSafe(32), tokenURLSafe(64)
	c.Redactor.Register(state)
	c.Redactor.Register(verifier)
	c.pendingState, c.pendingVerifier, c.pendingExpires = state, verifier, c.now()+600
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	params := []string{
		"client_id=" + url.QueryEscape(c.ClientID),
		"redirect_uri=" + url.QueryEscape(c.RedirectURI),
		"scope=" + url.QueryEscape(strings.Join(scopes, " ")),
		"state=" + url.QueryEscape(state),
		"response_type=code",
	}
	if c.provider.offlineParams {
		params = append(params, "access_type=offline", "prompt=consent")
	}
	params = append(params, "code_challenge="+url.QueryEscape(challenge), "code_challenge_method=S256")
	return c.provider.authorizeURL + strings.Join(params, "&"), nil
}

// Complete exchanges the callback code once; failures discard the attempt.
func (c *OAuthConnection) Complete(state, code string) error {
	if c.pendingState == "" || subtle.ConstantTimeCompare([]byte(state), []byte(c.pendingState)) != 1 || c.now() >= c.pendingExpires {
		return errors.New("invalid or expired " + c.provider.name + " connection request")
	}
	verifier := c.pendingVerifier
	c.pendingState, c.pendingVerifier, c.pendingExpires = "", "", 0
	if !authCodeShape(code) {
		return errors.New("invalid " + c.provider.name + " authorization code")
	}
	c.Redactor.Register(code)
	result, err := c.Transport(c.ClientID, c.ClientSecret, c.provider.tokenEndpoint, map[string]string{
		"code": code, "redirect_uri": c.RedirectURI, "grant_type": "authorization_code", "code_verifier": verifier})
	if err != nil {
		return err
	}
	if c.provider.name == figmaProvider.name {
		var userID string
		switch v := result["user_id_string"].(type) {
		case string:
			userID = v
		default:
			switch w := result["user_id"].(type) {
			case string:
				userID = w
			case json.Number:
				userID = string(w)
			case float64:
				userID = Dumps(w)
				userID = strings.TrimSuffix(userID, ".0")
			}
		}
		if !regexp.MustCompile(`^[0-9]{1,128}$`).MatchString(userID) {
			return errors.New("Figma did not return an account identity")
		}
		if err = c.Install(result, true); err != nil {
			return err
		}
		c.UserID = userID
		return nil
	}
	return c.Install(result, true)
}

// Install validates a token response and adopts it.
func (c *OAuthConnection) Install(data map[string]any, initial bool) error {
	if data == nil {
		return errors.New("invalid " + c.provider.name + " token response")
	}
	token, _ := data["access_token"].(string)
	refresh := c.RefreshToken
	if initial {
		refresh = ""
	}
	if v, ok := data["refresh_token"]; ok {
		refresh, _ = v.(string)
	}
	ttl, ttlOK := asNumber(data["expires_in"])
	tokenType := "bearer"
	if v, ok := data["token_type"]; ok {
		tokenType, _ = v.(string)
	}
	if !oauthTokenShape(token) || !oauthTokenShape(refresh) || !ttlOK || !(ttl > 60 && ttl <= 366*86400) || strings.ToLower(tokenType) != "bearer" {
		return errors.New("invalid " + c.provider.name + " token response")
	}
	if c.provider.requireScopes {
		scope, ok := data["scope"].(string)
		if !ok || !sameStringSet(strings.Fields(scope), c.provider.scopes) {
			return errors.New("unexpected " + c.provider.name + " OAuth scopes")
		}
	}
	if c.provider.subsetScopes {
		if v, present := data["scope"]; present {
			scope, ok := v.(string)
			if !ok {
				return errors.New("unexpected " + c.provider.name + " OAuth scopes")
			}
			allowed := map[string]bool{}
			for _, s := range c.provider.scopes {
				allowed[s] = true
			}
			for _, s := range strings.Fields(strings.ReplaceAll(scope, ",", " ")) {
				if !allowed[s] {
					return errors.New("unexpected " + c.provider.name + " OAuth scopes")
				}
			}
		}
	}
	c.Redactor.Register(token)
	c.Redactor.Register(refresh)
	c.AccessToken, c.RefreshToken, c.Expires = token, refresh, c.now()+ttl
	return nil
}

func sameStringSet(a, b []string) bool {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	other := map[string]bool{}
	for _, s := range b {
		other[s] = true
	}
	if len(set) != len(other) {
		return false
	}
	for s := range set {
		if !other[s] {
			return false
		}
	}
	return true
}

// Authorization returns a bearer header, refreshing first when needed.
func (c *OAuthConnection) Authorization() (string, error) {
	return c.authorizationWith(c.Install)
}

func (c *OAuthConnection) authorizationWith(install func(map[string]any, bool) error) (string, error) {
	if c.AccessToken == "" {
		return "", errors.New("connect your " + c.provider.name + " account in Warden")
	}
	if c.now() >= c.Expires-60 {
		fields := map[string]string{"refresh_token": c.RefreshToken}
		if c.provider.offlineParams {
			fields["grant_type"] = "refresh_token"
		}
		result, err := c.Transport(c.ClientID, c.ClientSecret, c.provider.refreshEndpoint, fields)
		if err != nil {
			return "", err
		}
		if err = install(result, false); err != nil {
			return "", err
		}
	}
	return "Bearer " + c.AccessToken, nil
}

func (c *OAuthConnection) Disconnect() {
	c.pendingState, c.pendingVerifier, c.pendingExpires = "", "", 0
	c.AccessToken, c.RefreshToken, c.UserID = "", "", ""
	c.Expires = 0
}

func (c *OAuthConnection) Connected() bool { return c.AccessToken != "" }

// exchange is the fixed HTTPS token request: fixed authority, bounded JSON,
// no redirects and no proxy environment.
func (c *OAuthConnection) exchange(clientID, clientSecret, endpoint string, fields map[string]string) (map[string]any, error) {
	if endpoint != c.provider.tokenEndpoint && endpoint != c.provider.refreshEndpoint {
		return nil, errors.New("unsupported OAuth endpoint")
	}
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}
	if c.provider.basicAuth {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret))
	} else {
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	status, data, err := directHTTPS(c.provider.tokenHost, "POST", endpoint, []byte(form.Encode()), headers, 10*time.Second, 65537)
	if err != nil || status != 200 || len(data) > 65536 {
		if err != nil {
			return nil, errors.New(c.provider.name + " authorization unavailable; retry connecting")
		}
		return nil, errors.New(c.provider.name + " authorization failed; reconnect your account")
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, errors.New(c.provider.name + " authorization unavailable; retry connecting")
	}
	return result, nil
}

// directHTTPS issues one request to a fixed host with system TLS roots, no
// proxy, no redirects and a bounded body. Returned data may exceed limit
// by one byte so callers can detect overflow.
func directHTTPS(host, method, path string, body []byte, headers map[string]string, timeout time.Duration, limit int64) (int, []byte, error) {
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: timeout, DisableCompression: true, ForceAttemptHTTP2: false}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, reader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return 0, nil, err
	}
	return res.StatusCode, data, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(cryptoRandReader, b); err != nil {
		panic(err)
	}
	return b
}
