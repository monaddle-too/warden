package cloudauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type memoryMailer struct{ code string }

func (m *memoryMailer) SendCode(_ context.Context, _, code string) error { m.code = code; return nil }

func TestEmailAndNames(t *testing.T) {
	if got, err := NormalizeEmail(" Alice@Example.Com "); err != nil || got != "alice@example.com" {
		t.Fatalf("normalization: %q %v", got, err)
	}
	for _, email := range []string{"Alice <alice@example.com>", "a@example.com\r\nBcc: x@y.com", "@example.com", "bad"} {
		if _, err := NormalizeEmail(email); err == nil {
			t.Errorf("accepted %q", email)
		}
	}
	if _, err := ValidName("\nInjected"); err == nil {
		t.Fatal("accepted line break in a name")
	}
}

// Run with WARDEN_TEST_POSTGRES_URL pointing at an isolated disposable DB.
// It proves membership revocation invalidates a live browser session and a
// used email code cannot create a second one.
func TestCloudIdentityPostgres(t *testing.T) {
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
	owner, err := store.UserByEmail(ctx, "owner@example.com")
	if err != nil || !owner.FullAdmin {
		t.Fatalf("bootstrap owner: %+v %v", owner, err)
	}
	legacyID := ID()
	if err = store.EnsureOrganization(ctx, owner, legacyID, "Legacy "+legacyID); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureOrganization(ctx, owner, legacyID, "Legacy "+legacyID); err != nil {
		t.Fatalf("bootstrap was not idempotent: %v", err)
	}
	legacyMembers, err := store.Members(ctx, owner, legacyID)
	if err != nil || len(legacyMembers) != 1 || legacyMembers[0].Email != owner.Email || legacyMembers[0].Role != "admin" {
		t.Fatalf("initial organization members: %+v %v", legacyMembers, err)
	}
	org, err := store.CreateOrganization(ctx, owner, "Organization "+ID())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.PutMember(ctx, owner, org.ID, "alice@example.com", "Alice Person", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutMember(ctx, alice.User, org.ID, "alice@example.com", "Another Name", "user"); err == nil {
		t.Fatal("organization admin demoted an admin")
	}
	if err = store.RemoveMember(ctx, alice.User, org.ID, "alice@example.com"); err == nil {
		t.Fatal("organization admin removed an admin")
	}
	mail := &memoryMailer{}
	login := Login{Store: store, Mailer: mail, Pepper: []byte("0123456789abcdef0123456789abcdef")}
	if err = login.IssueCode(ctx, "alice@example.com", "198.51.100.1"); err != nil {
		t.Fatal(err)
	}
	if len(mail.code) != 8 {
		t.Fatal("no code sent")
	}
	session, raw, err := login.VerifyCode(ctx, "alice@example.com", mail.code)
	if err != nil {
		t.Fatal(err)
	}
	if session.OrganizationID != org.ID || session.Role != "admin" {
		t.Fatalf("wrong organization/role: %+v", session)
	}
	if _, _, err = login.VerifyCode(ctx, "alice@example.com", mail.code); err == nil {
		t.Fatal("replayed code")
	}
	if _, err = store.Session(ctx, raw); err != nil {
		t.Fatal(err)
	}
	auth := Auth{Login: &login, Origin: "https://cloud.example.com"}
	request := httptest.NewRequest(http.MethodPost, "https://cloud.example.com/api/chats", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	request.Header.Set("Origin", auth.Origin)
	if auth.Role(request) != "" {
		t.Fatal("write accepted without CSRF")
	}
	request.Header.Set("X-Warden-CSRF", session.CSRF)
	if auth.Role(request) != "user" {
		t.Fatal("organization admin lost user rights")
	}
	challenge, err := store.passkeyChallenge(ctx, session.User.ID, "login", &webauthn.SessionData{Challenge: ID(), Expires: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	userID, _, err := store.takePasskeyChallenge(ctx, challenge, "login")
	if err != nil || userID != session.User.ID {
		t.Fatalf("passkey challenge: %s %v", userID, err)
	}
	if _, _, err = store.takePasskeyChallenge(ctx, challenge, "login"); err == nil {
		t.Fatal("replayed passkey challenge")
	}
	if err = store.RemoveMember(ctx, owner, org.ID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Session(ctx, raw); err == nil {
		t.Fatal("removed member retained a session")
	}
	if err = store.RemoveMember(ctx, owner, org.ID, "alice@example.com"); err == nil {
		t.Fatal("removed absent member twice")
	}
}

// Exercise the actual WebAuthn-generated sessions: a hand-built Expires in a
// storage-only test misses client-only timeout defaults (zero server expiry).
func TestPasskeyCeremonyExpiry(t *testing.T) {
	auth := Auth{Origin: "https://cloud.example.com"}
	wa, err := auth.webauthn()
	if err != nil {
		t.Fatal(err)
	}
	u := passkeyUser{User: User{ID: ID(), Email: "passkey@example.com", Name: "Passkey Tester"}, Credentials: []webauthn.Credential{{ID: []byte("existing-credential")}}}
	_, registration, err := wa.BeginRegistration(u)
	if err != nil {
		t.Fatal(err)
	}
	_, login, err := wa.BeginLogin(u)
	if err != nil {
		t.Fatal(err)
	}
	for kind, session := range map[string]*webauthn.SessionData{"register": registration, "login": login} {
		t.Run(kind, func(t *testing.T) {
			remaining := time.Until(session.Expires)
			if remaining < 4*time.Minute || remaining > 5*time.Minute {
				t.Fatalf("new challenge has invalid expiry: %v", session.Expires)
			}
			if dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL"); dsn != "" {
				ctx := context.Background()
				store, err := Open(ctx, dsn, "owner@example.com")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				owner, err := store.UserByEmail(ctx, "owner@example.com")
				if err != nil {
					t.Fatal(err)
				}
				id, err := store.passkeyChallenge(ctx, owner.ID, kind, session)
				if err != nil {
					t.Fatal(err)
				}
				_, got, err := store.takePasskeyChallenge(ctx, id, kind)
				if err != nil || got.Challenge != session.Challenge {
					t.Fatalf("fresh challenge cannot finish: %v", err)
				}
				if _, _, err := store.takePasskeyChallenge(ctx, id, kind); err == nil {
					t.Fatal("challenge replay accepted")
				}
				session.Expires = time.Now().Add(-time.Minute)
				id, err = store.passkeyChallenge(ctx, owner.ID, kind, session)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.takePasskeyChallenge(ctx, id, kind); err == nil {
					t.Fatal("expired challenge accepted")
				}
			}
		})
	}
}
