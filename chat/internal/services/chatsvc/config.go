package chatsvc

import (
	"flag"
	"os"
	"path/filepath"

	"warden/chat/internal/config"
	"warden/chat/internal/transport"
)

// settings are the effective chat values after reconciling the legacy flags
// with the optional warden.json. Each flag maps to one field; a flag that
// disagrees with a loaded file is an error.
type settings struct {
	cfg           config.Config
	configPath    string         // the warden.json the settings came from ("" for flags only)
	state         string         // paths.state/app
	policy        string         // services.policy.address (unix://paths.state/policy/sbx-control.sock)
	runner        string         // services.runner.address (unix://paths.state/runner/worker.sock)
	listen        string         // chat.listen, the loopback host:port
	listenURL     string         // services.chat.listen (http://<chat.listen>) or tls://
	address       string         // services.chat.address: what the edge dials; its host is this server's name
	tls           *transport.TLS // tls.*, present when any transport URL is tls://
	web           string         // paths.webAssets
	suffix        string         // previews.hostSuffix; "" leaves previews unconfigured
	previewScheme string         // from previews.mode
	previewPort   string         // previews.edgeListen port in loopback mode
	// runnerPreviews is services.runner.previews.address, the runner's
	// shared mutual-TLS preview server the ports proxy dials; "" on the
	// sbx shapes (loopback attachment URLs). File only.
	runnerPreviews string
}

type chatFlags struct {
	configPath, state, wardenSocket, runnerSocket, listen, web, suffix *string
}

func resolveSettings(fs *flag.FlagSet, f chatFlags) (settings, error) {
	cfg, source, err := config.Resolve(*f.configPath, *f.state)
	if err != nil {
		return settings{}, err
	}
	o := config.NewOverrides(fs, source)
	s := settings{cfg: cfg, configPath: source}
	s.state = config.Override(o, "state", *f.state, "paths.state (app directory)", cfg.AppState())
	s.policy = config.Override(o, "warden-socket", "unix://"+*f.wardenSocket, "services.policy.address", cfg.PolicyAddress())
	s.runner = config.Override(o, "runner-socket", "unix://"+*f.runnerSocket, "services.runner.address", cfg.RunnerAddress())
	s.listen = config.Override(o, "listen", *f.listen, "chat.listen", cfg.Chat.Listen)
	s.listenURL = config.Override(o, "listen", "http://"+*f.listen, "services.chat.listen", cfg.ChatListen())
	s.address = cfg.ChatAddress()
	if o.Set("listen") && !o.FromFile() {
		s.address = s.listenURL
	}
	s.runnerPreviews = cfg.RunnerPreviewAddress()
	s.tls = cfg.TransportTLS()
	s.web = config.Override(o, "web-dir", *f.web, "paths.webAssets", cfg.Paths.WebAssets)
	if s.web == "" {
		s.web = defaultWebDir()
	}
	s.suffix = config.Override(o, "preview-suffix", *f.suffix, "previews.hostSuffix", cfg.Previews.HostSuffix)
	if o.Set("preview-suffix") && !o.FromFile() {
		// The legacy flag alone decides the preview shape: an empty suffix
		// leaves external previews unconfigured, "localhost" is loopback,
		// a dotted suffix is public.
		switch s.suffix {
		case "":
		case "localhost":
			cfg.Previews.Mode = config.PreviewLoopback
		default:
			cfg.Previews.Mode = config.PreviewPublic
		}
		s.cfg = cfg
	}
	if err := o.Err(); err != nil {
		return settings{}, err
	}
	s.previewScheme = cfg.PreviewScheme()
	if cfg.Previews.Mode == config.PreviewLoopback {
		s.previewPort = cfg.EdgePort()
	}
	return s, nil
}

// defaultWebDir finds the built UI when neither the file nor a flag names
// it: the container layout (/app/web), the release layout ("web" beside the
// "bin" directory holding the binary), a "web" directory beside the binary,
// or chat/web/dist in a source checkout two levels above the binary
// (dist/chat/warden-chat). The last candidate is returned even if absent so
// the startup error names a concrete path.
func defaultWebDir() string {
	candidates := []string{"/app/web"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "..", "web"), filepath.Join(dir, "web"), filepath.Join(dir, "..", "..", "chat", "web", "dist"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !info.IsDir() {
			return filepath.Clean(c)
		}
	}
	return "chat/web/dist"
}
