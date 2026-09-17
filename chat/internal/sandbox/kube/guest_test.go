package kube

import (
	"archive/tar"
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

// guestManifestJSON is the base image's manifest as deploy/guest/README.md
// documents it.
const guestManifestJSON = `{"platform":"linux/arm64","variant":"base","codex":{"version":"0.154.0","target":"aarch64-unknown-linux-musl"},"claude":{"version":"2.1.272","sha256":"` + "0000000000000000000000000000000000000000000000000000000000000000" + `"},"ca":{"sha256":"1111111111111111111111111111111111111111111111111111111111111111"},"paths":{"codex":"/opt/warden/runtime","claude":"/opt/warden/claude/claude","trust":"/opt/warden/trust/ca-certificates.crt","home":"/home/agent"},"user":{"name":"agent","uid":1000,"gid":1000}}`

// fakeGuest scripts what exec finds in a pod: the manifest, the workspace
// directory, the home for a fork, tar extraction for copies, and an agent
// that echoes its input. Every call is recorded.
type fakeGuest struct {
	api *fakeAPI
	mu  sync.Mutex
	// manifest is what the manifest path holds; empty means no file.
	manifest string
	// workspaces marks pods whose workspace directory exists.
	workspaces map[string]bool
	// homes is the content each pod's home tars up for a fork.
	homes map[string]map[string]string
	// copied holds what a pod extracted into its home (a fork's copy);
	// received holds the root-owned entries a pod extracted for Copy.
	copied   map[string]map[string]string
	received map[string][]tarEntry
	calls    []guestCall
	// hook answers a command first; handled false falls through.
	hook func(pod string, command []string, stdin io.Reader, stdout, stderr io.Writer) (code int, handled bool)
}

type guestCall struct {
	Pod     string
	Dir     string
	Command []string
}

type tarEntry struct {
	Name    string
	Mode    int64
	UID     int
	Type    byte
	Content string
}

func newFakeGuest(api *fakeAPI) *fakeGuest {
	return &fakeGuest{api: api, manifest: guestManifestJSON, workspaces: map[string]bool{}, homes: map[string]map[string]string{}, copied: map[string]map[string]string{}, received: map[string][]tarEntry{}}
}

func (g *fakeGuest) recorded() []guestCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]guestCall(nil), g.calls...)
}

// unwrap strips the driver's working-directory wrapper.
func unwrap(command []string) (dir string, inner []string) {
	if len(command) >= 5 && command[0] == "sh" && command[1] == "-c" && command[2] == `cd "$0" && exec "$@"` {
		return command[3], command[4:]
	}
	return "", command
}

func (g *fakeGuest) exec(pod string, command []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, inner := unwrap(command)
	g.mu.Lock()
	g.calls = append(g.calls, guestCall{Pod: pod, Dir: dir, Command: inner})
	hook := g.hook
	manifest := g.manifest
	g.mu.Unlock()
	if hook != nil {
		if code, handled := hook(pod, inner, stdin, stdout, stderr); handled {
			return code
		}
	}
	joined := strings.Join(inner, " ")
	switch {
	case joined == "cat "+GuestManifestPath:
		if manifest == "" {
			io.WriteString(stderr, "cat: "+GuestManifestPath+": No such file or directory\n")
			return 1
		}
		io.WriteString(stdout, manifest)
		return 0
	case len(inner) == 3 && inner[0] == "test" && inner[1] == "-d":
		g.mu.Lock()
		ok := g.workspaces[pod]
		g.mu.Unlock()
		if ok {
			return 0
		}
		return 1
	case len(inner) == 4 && inner[0] == "sh" && inner[1] == "-c" && strings.Contains(inner[2], "WARDEN-GUEST-BEGIN"):
		// The worker's guest report: mkdir -p of the workspace, then the
		// manifest between the markers and the presence lines.
		g.mu.Lock()
		g.workspaces[pod] = true
		g.mu.Unlock()
		io.WriteString(stdout, "WARDEN-GUEST-BEGIN\n"+manifest+"\n\nWARDEN-GUEST-END\nca-absent\ncodex-present\nclaude-present\n")
		return 0
	case len(inner) == 6 && inner[0] == "tar" && inner[1] == "-C" && inner[3] == "-cf" && inner[4] == "-" && inner[5] == ".":
		g.mu.Lock()
		home := g.homes[pod]
		g.mu.Unlock()
		if home == nil {
			io.WriteString(stderr, "tar: no home\n")
			return 2
		}
		tw := tar.NewWriter(stdout)
		for name, content := range home {
			_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(content)), Uid: 1000, Gid: 1000})
			_, _ = io.WriteString(tw, content)
		}
		_ = tw.Close()
		return 0
	case len(inner) == 5 && inner[0] == "tar" && inner[1] == "-C" && inner[3] == "-xf" && inner[4] == "-":
		files := map[string]string{}
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			content, _ := io.ReadAll(tr)
			files[strings.TrimPrefix(h.Name, "./")] = string(content)
		}
		g.mu.Lock()
		g.copied[pod] = files
		g.workspaces[pod] = true
		g.mu.Unlock()
		return 0
	case len(inner) == 6 && inner[0] == "sudo" && inner[1] == "-n" && inner[2] == "sh" && inner[3] == "-c" && strings.Contains(inner[4], "tar -xf - -C"):
		var entries []tarEntry
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			content, _ := io.ReadAll(tr)
			entries = append(entries, tarEntry{Name: inner[5] + "/" + h.Name, Mode: h.Mode, UID: h.Uid, Type: h.Typeflag, Content: string(content)})
		}
		g.mu.Lock()
		g.received[pod] = append(g.received[pod], entries...)
		g.mu.Unlock()
		return 0
	case len(inner) > 0 && inner[0] == "env" && (strings.Contains(joined, "app-server") || strings.Contains(joined, "--input-format")):
		// The agent: echoes each input line until its input ends.
		lines := bufio.NewScanner(stdin)
		io.WriteString(stdout, "agent-ready\n")
		for lines.Scan() {
			io.WriteString(stdout, "agent:"+lines.Text()+"\n")
		}
		return 0
	case len(inner) > 0 && inner[0] == "sudo":
		return 0
	case len(inner) > 0 && inner[0] == "cat":
		_, _ = io.Copy(stdout, stdin)
		return 0
	case len(inner) >= 3 && inner[0] == "sh" && inner[1] == "-c" && strings.HasPrefix(inner[2], "exit "):
		code := 0
		for _, c := range inner[2][len("exit "):] {
			code = code*10 + int(c-'0')
		}
		return code
	case len(inner) > 0 && inner[0] == "yes":
		// Unbounded output for the limit test.
		chunk := bytes.Repeat([]byte("y\n"), 32<<10)
		for i := 0; i < 64; i++ {
			if _, err := stdout.Write(chunk); err != nil {
				return 141
			}
		}
		return 0
	}
	return 0
}
