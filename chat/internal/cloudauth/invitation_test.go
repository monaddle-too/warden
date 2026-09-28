package cloudauth

import (
	"strings"
	"testing"
)

func TestInvitationText(t *testing.T) {
	for _, role := range []string{"admin", "user"} {
		text := invitationText("member@example.com", "Device Lab", role, "https://warden.example.com")
		for _, want := range []string{"Device Lab", "member@example.com", "https://warden.example.com", "one-time sign-in code", "Manage devices"} {
			if !strings.Contains(text, want) {
				t.Fatalf("invitation missing %q", want)
			}
		}
		if strings.Contains(text, "organization administrator") != (role == "admin") {
			t.Fatal("incorrect invitation role")
		}
	}
}
