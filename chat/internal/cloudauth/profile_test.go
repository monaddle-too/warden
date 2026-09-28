package cloudauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// A software authenticator exercises actual WebAuthn verification, not a mock
// acceptance path. No personal/device credentials are involved.
func TestPersonalSettingsPostgres(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("WARDEN_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, _ := store.UserByEmail(ctx, "owner@example.com")
	org, err := store.CreateOrganization(ctx, owner, "Settings "+ID())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.PutMember(ctx, owner, org.ID, ID()+"@example.com", "Alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.PutMember(ctx, owner, org.ID, ID()+"@example.com", "Bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	aliceSession, aliceToken, err := store.newPasskeySession(ctx, alice.User)
	if err != nil {
		t.Fatal(err)
	}
	bobSession, bobToken, err := store.newPasskeySession(ctx, bob.User)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Auth{Login: &Login{Store: store}, Origin: "https://cloud.example.com"}
	handler := auth.Handler()
	request := func(method, path string, body any, token, csrf string, want int) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, auth.Origin+path, bytes.NewReader(data))
		r.Header.Set("Origin", auth.Origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Warden-CSRF", csrf)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	call := func(method, path string, body any, want int) *httptest.ResponseRecorder {
		return request(method, path, body, aliceToken, aliceSession.CSRF, want)
	}
	offer := func(want bool) {
		t.Helper()
		w := call("GET", "/auth/session", nil, 200)
		var v struct {
			Offer bool `json:"offerPasskey"`
		}
		json.Unmarshal(w.Body.Bytes(), &v)
		if v.Offer != want {
			t.Fatalf("offerPasskey=%v, want %v", v.Offer, want)
		}
	}
	offer(true)
	request("POST", "/auth/profile", map[string]string{"name": "No CSRF"}, aliceToken, "", 403)
	request("POST", "/auth/profile", map[string]string{"name": "Anonymous"}, "", "", 401)
	call("POST", "/auth/profile", map[string]string{"name": "   "}, 400)
	call("POST", "/auth/profile", map[string]any{"name": "Alice New", "fullAdmin": true}, 400)
	call("POST", "/auth/profile", map[string]string{"name": "Alice New"}, 200)
	fresh, err := store.Session(ctx, aliceToken)
	if err != nil || fresh.User.Name != "Alice New" || fresh.User.FullAdmin {
		t.Fatalf("profile not reflected in session: %+v %v", fresh.User, err)
	}
	other, _ := store.User(ctx, bob.ID)
	if other.Name != "Bob" {
		t.Fatal("changed another user")
	}
	call("POST", "/auth/profile/passkey-prompt", nil, 200)
	offer(false)
	// The choice survives another session, not only one browser render.
	_, secondToken, err := store.newPasskeySession(ctx, alice.User)
	if err != nil {
		t.Fatal(err)
	}
	w := request("GET", "/auth/session", nil, secondToken, "", 200)
	var state map[string]any
	json.Unmarshal(w.Body.Bytes(), &state)
	if state["offerPasskey"] != false {
		t.Fatal("skip was not durable")
	}

	type challenge struct {
		ID      string `json:"id"`
		Options struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	begin := func(replace string) challenge {
		t.Helper()
		w := call("POST", "/auth/passkeys/register/start", map[string]string{"name": "Laptop", "replaceID": replace}, 200)
		var c challenge
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	encode := base64.RawURLEncoding.EncodeToString
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte("cloud.example.com"))
	registration := func(c challenge, id []byte) map[string]any {
		client, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": c.Options.PublicKey.Challenge, "origin": auth.Origin})
		pub, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
		data := append([]byte{}, rpHash[:]...)
		data = append(data, 0x45, 0, 0, 0, 0)
		data = append(data, make([]byte, 16)...)
		data = binary.BigEndian.AppendUint16(data, uint16(len(id)))
		data = append(data, id...)
		data = append(data, pub...)
		attestation, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": data})
		return map[string]any{"id": c.ID, "credential": map[string]any{"id": encode(id), "rawId": encode(id), "type": "public-key", "response": map[string]any{"clientDataJSON": encode(client), "attestationObject": encode(attestation)}}}
	}
	firstID := []byte(ID())
	first := begin("")
	call("POST", "/auth/passkeys/register/finish", registration(first, firstID), 200)
	call("POST", "/auth/passkeys/register/finish", registration(first, firstID), 403)
	offer(false)
	keys, err := store.passkeys(ctx, alice.ID)
	if err != nil || len(keys) != 1 || keys[0].Name != "Laptop" {
		t.Fatalf("saved key: %+v %v", keys, err)
	}
	// A failed insert after removing the replacement target must roll back.
	stored, err := store.passkeyUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	conflict := stored.Credentials[0]
	conflict.ID = []byte(ID())
	if err = store.savePasskey(ctx, bob.ID, "Bob key", nil, &conflict); err != nil {
		t.Fatal(err)
	}
	if err = store.savePasskey(ctx, alice.ID, "Conflicting replacement", firstID, &conflict); err == nil {
		t.Fatal("accepted another user's credential ID")
	}
	keys, _ = store.passkeys(ctx, alice.ID)
	if len(keys) != 1 || keys[0].ID != encode(firstID) {
		t.Fatal("failed insert lost the old credential")
	}
	profile := call("GET", "/auth/profile", nil, 200)
	var listed struct {
		Passkeys []passkeyInfo `json:"passkeys"`
	}
	if err = json.Unmarshal(profile.Body.Bytes(), &listed); err != nil || len(listed.Passkeys) != 1 || listed.Passkeys[0].ID != encode(firstID) {
		t.Fatal("profile leaked another user's keys")
	}
	path := "/auth/passkeys/" + encode(firstID)
	request("DELETE", path, nil, bobToken, bobSession.CSRF, 404)
	request("PATCH", path, map[string]string{"name": "stolen"}, bobToken, bobSession.CSRF, 404)
	request("DELETE", path, nil, aliceToken, "", 403)
	request("POST", "/auth/passkeys/register/start", map[string]string{"name": "stolen", "replaceID": encode(firstID)}, bobToken, bobSession.CSRF, 503)
	call("PATCH", path, map[string]string{"name": "Work laptop"}, 200)
	// Invalid new attestation leaves the original credential in place.
	failed := begin(encode(firstID))
	call("POST", "/auth/passkeys/register/finish", map[string]any{"id": failed.ID, "credential": map[string]any{}}, 400)
	keys, _ = store.passkeys(ctx, alice.ID)
	if len(keys) != 1 || keys[0].Name != "Work laptop" {
		t.Fatal("failed replacement removed old passkey")
	}
	replacement := begin(encode(firstID))
	secondID := []byte(ID())
	call("POST", "/auth/passkeys/register/finish", registration(replacement, secondID), 200)
	keys, _ = store.passkeys(ctx, alice.ID)
	if len(keys) != 1 || keys[0].ID != encode(secondID) {
		t.Fatal("replacement was not atomic")
	}
	call("DELETE", path, nil, 404)
	// Sign in with the replacement using a real signed assertion.
	w = call("POST", "/auth/passkeys/login/start", map[string]string{"email": alice.Email}, 200)
	var login challenge
	json.Unmarshal(w.Body.Bytes(), &login)
	client, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": login.Options.PublicKey.Challenge, "origin": auth.Origin})
	data := append([]byte{}, rpHash[:]...)
	data = append(data, 0x05, 0, 0, 0, 1)
	clientHash := sha256.Sum256(client)
	signed := append(append([]byte{}, data...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := hex.DecodeString(alice.ID)
	assertion := map[string]any{"id": login.ID, "credential": map[string]any{"id": encode(secondID), "rawId": encode(secondID), "type": "public-key", "response": map[string]any{"clientDataJSON": encode(client), "authenticatorData": encode(data), "signature": encode(signature), "userHandle": encode(handle)}}}
	w = call("POST", "/auth/passkeys/login/finish", assertion, 200)
	if len(w.Result().Cookies()) != 1 {
		t.Fatal("passkey login created no session")
	}
	keys, _ = store.passkeys(ctx, alice.ID)
	if keys[0].LastUsedAt == nil {
		t.Fatal("missing last use")
	}
	call("DELETE", "/auth/passkeys/"+encode(secondID), nil, 200)
	keys, _ = store.passkeys(ctx, alice.ID)
	if len(keys) != 0 {
		t.Fatal("passkey not removed")
	}
	offer(false)
	call("POST", "/auth/passkeys/login/start", map[string]string{"email": alice.Email}, 404)
	// Existing signed-in sessions and the account's email path remain intact.
	if _, err = store.Session(ctx, aliceToken); err != nil {
		t.Fatal(err)
	}
}
