package hostinfo

import (
	"errors"
	"net"
	"strconv"
	"testing"

	"warden/chat/internal/release"
)

func TestFindSBXPrefersPathThenFixedLocations(t *testing.T) {
	onPath := func(string) (string, error) { return "/usr/local/bin/sbx", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if got, err := FindSBX(onPath, func(string) bool { return false }); err != nil || got != "/usr/local/bin/sbx" {
		t.Fatal(got, err)
	}
	if got, err := FindSBX(missing, func(p string) bool { return p == "/usr/bin/sbx" }); err != nil || got != "/usr/bin/sbx" {
		t.Fatal(got, err)
	}
	if got, err := FindSBX(missing, func(p string) bool { return true }); err != nil || got != "/opt/homebrew/bin/sbx" {
		t.Fatal("homebrew location must come before /usr/bin", got, err)
	}
	if _, err := FindSBX(missing, func(string) bool { return false }); err == nil {
		t.Fatal("missing sbx accepted")
	}
}

func TestSBXVersionParsing(t *testing.T) {
	for _, tc := range []struct {
		output, version string
		ok              bool
	}{
		{"sbx version: v" + release.SBXTestedVersion + " (build abc)\n", release.SBXTestedVersion, true},
		{"sbx version: v" + release.SBXTestedVersion + "\n", release.SBXTestedVersion, true},
		{"sbx version: v0.41.0 (build abc)", "0.41.0", true},
		{"docker sandbox 1.0", "", false},
		{"sbx version: ", "", false},
		{"", "", false},
	} {
		got, err := ParseSBXVersion(tc.output)
		if (err == nil) != tc.ok || got != tc.version {
			t.Fatalf("%q: %q %v", tc.output, got, err)
		}
	}
	if err := CheckSBXVersion("sbx version: v" + release.SBXTestedVersion + " x"); err != nil {
		t.Fatal(err)
	}
	if err := CheckSBXVersion("sbx version: v0.41.0 x"); err != nil {
		t.Fatal("wrong SBX version accepted")
	}
}

func TestGuestArchitecture(t *testing.T) {
	if GuestArchFor("arm64") != release.ARM64 || GuestArchFor("amd64") != release.AMD64 || GuestArchFor("386") != "" {
		t.Fatal("unexpected guest architecture mapping")
	}
	if GuestArch() == "" {
		t.Skip("unsupported test host architecture")
	}
}

func TestSizing(t *testing.T) {
	const gib = 1 << 30
	for _, tc := range []struct {
		memory uint64
		cores  int
		want   Sizing
	}{
		{2 * gib, 2, Sizing{1536, 1, 0}},
		{4 * gib, 2, Sizing{1536, 1, 0}},
		{8 * gib, 4, Sizing{1536, 1, 1}},
		{12 * gib, 4, Sizing{1536, 2, 1}}, // the OVH shape: two running, one spare
		{16 * gib, 10, Sizing{1536, 3, 1}},
		{16 * gib, 2, Sizing{1536, 1, 1}}, // cores cap running, memory still allows the spare
		{64 * gib, 4, Sizing{1536, 3, 1}},
		{128 * gib, 64, Sizing{1536, 8, 1}},
		{0, 0, Sizing{1536, 1, 0}},
	} {
		if got := Size(tc.memory, tc.cores); got != tc.want {
			t.Fatalf("%d GiB, %d cores: %+v, want %+v", tc.memory/gib, tc.cores, got, tc.want)
		}
	}
}

func TestFreePortsAreDistinctLoopbackPorts(t *testing.T) {
	ports, err := FreePorts(3)
	if err != nil || len(ports) != 3 {
		t.Fatal(ports, err)
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if p < 1024 || p > 65535 || seen[p] {
			t.Fatal("bad or duplicate port", ports)
		}
		seen[p] = true
		l, err := net.Listen("tcp4", Loopback(p))
		if err != nil {
			t.Fatal("port not free", p, err)
		}
		l.Close()
	}
	if Loopback(18781) != "127.0.0.1:"+strconv.Itoa(18781) {
		t.Fatal(Loopback(18781))
	}
}
