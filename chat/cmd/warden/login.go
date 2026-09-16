package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"warden/chat/internal/login"

	"warden/chat/internal/config"
	"warden/chat/internal/release"
)

// login stores one provider sign-in as exactly one owner-only (0600) file in
// the provider directory warden.json names. An existing file is never
// replaced unless --replace is given.
func (c *cli) login(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "usage: warden login codex|claude|github [flags]")
		return errUsage
	}
	provider, rest := args[0], args[1:]
	fs := flag.NewFlagSet("warden login "+provider, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	replace := fs.Bool("replace", false, "replace an existing sign-in file")
	var codexCLI, pasteToken string
	var pasteStdin bool
	switch provider {
	case "codex":
		fs.StringVar(&codexCLI, "codex-cli", "", "a host-native codex executable that runs `login --device-auth` (default on Linux: the installed bundle's bin/codex)")
	case "github":
		fs.StringVar(&pasteToken, "paste", "", "store this token instead of running the device flow (for example `gh auth token`); prefer --paste-stdin")
		fs.BoolVar(&pasteStdin, "paste-stdin", false, "read a token to store from standard input")
	case "claude":
	default:
		fmt.Fprintf(c.stderr, "warden login: unknown provider %q (codex, claude or github)\n", provider)
		return errUsage
	}
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	if err = ensurePrivateDir(cfg.ProviderDir()); err != nil {
		return err
	}
	switch provider {
	case "codex":
		if cfg.Providers.Codex == nil {
			return errors.New("warden.json disables the codex provider")
		}
		return c.loginCodex(cfg, codexCLI, *replace)
	case "claude":
		if cfg.Providers.Claude == nil {
			return errors.New("warden.json disables the claude provider")
		}
		return c.loginClaude(cfg.Providers.Claude.AuthFile, *replace)
	default:
		if cfg.GitHubMode() != "user" {
			return errors.New("warden.json does not configure GitHub with a user token (providers.github.authFile)")
		}
		return c.loginGitHub(cfg.Providers.GitHub.AuthFile, pasteToken, pasteStdin, *replace)
	}
}

// refuseExisting keeps an existing login unless replace is set.
func refuseExisting(path string, replace bool) error {
	if _, err := os.Stat(path); err == nil && !replace {
		return fmt.Errorf("%s already holds a sign-in; pass --replace to overwrite it", path)
	}
	return nil
}

// loginCodex runs the Codex CLI device login with CODEX_HOME set to a
// private scratch directory so only auth.json is produced, then moves that
// one file into place. The pinned bundle's bin/codex is a Linux executable
// (native on a Linux host); on macOS the operator names a host codex with
// --codex-cli or pastes the path of an auth.json obtained elsewhere.
func (c *cli) loginCodex(cfg config.Config, codexCLI string, replace bool) error {
	authFile := cfg.Providers.Codex.AuthFile
	if err := refuseExisting(authFile, replace); err != nil {
		return err
	}
	if codexCLI == "" && runtime.GOOS == "linux" {
		if candidate := filepath.Join(cfg.Runtimes.Codex, "bin", "codex"); executableFile(candidate) == nil {
			codexCLI = candidate
		}
	}
	scratch := filepath.Join(cfg.ProviderDir(), ".codex-login")
	if codexCLI != "" {
		if err := executableFile(codexCLI); err != nil {
			return fmt.Errorf("--codex-cli %s: %w", codexCLI, err)
		}
		os.RemoveAll(scratch)
		if err := ensurePrivateDir(scratch); err != nil {
			return err
		}
		defer os.RemoveAll(scratch)
		fmt.Fprintf(c.stdout, "Running %s login --device-auth with CODEX_HOME=%s; approve the device code in a browser signed in to the ChatGPT account Warden should use.\n", codexCLI, scratch)
		cmd := exec.Command(codexCLI, "login", "--device-auth")
		cmd.Env = append(os.Environ(), "CODEX_HOME="+scratch)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("codex login --device-auth: %w", err)
		}
		return c.importCodexAuth(filepath.Join(scratch, "auth.json"), authFile)
	}
	fmt.Fprintf(c.stdout, `Warden's Codex sign-in is the auth.json that `+"`codex login --device-auth`"+` writes. No host codex was given (--codex-cli PATH), so run it yourself, on any machine with a Codex CLI %s, with a private CODEX_HOME so only Warden's file is written:

    mkdir -m 700 -p %s
    CODEX_HOME=%s codex login --device-auth

then enter the path of the resulting auth.json (%s/auth.json) below. It is copied, mode 0600, to %s and the source can be deleted.

`, release.CodexVersion, scratch, scratch, scratch, authFile)
	fmt.Fprint(c.stdout, "Path to auth.json: ")
	line, err := readLine(c.stdin)
	if err != nil {
		return err
	}
	source := strings.TrimSpace(line)
	if source == "" {
		return errors.New("no path entered")
	}
	if source == filepath.Join(scratch, "auth.json") {
		defer os.RemoveAll(scratch)
	}
	return c.importCodexAuth(source, authFile)
}

// importCodexAuth copies one auth.json (a JSON object holding tokens) into
// the provider directory, mode 0600.
func (c *cli) importCodexAuth(source, authFile string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil || len(doc) == 0 {
		return fmt.Errorf("%s is not a Codex auth.json (a JSON object with the login tokens)", source)
	}
	if _, ok := doc["tokens"]; !ok {
		if _, ok = doc["OPENAI_API_KEY"]; !ok {
			return fmt.Errorf("%s has neither ChatGPT tokens nor an API key; run `codex login --device-auth` again", source)
		}
	}
	if err = atomicWrite(authFile, raw, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "wrote %s (mode 0600)\n", authFile)
	return nil
}

// claudeTokenShape is what scripts/warden-claude-token accepted:
// [A-Za-z0-9_.-]{16,4096} (Go's regexp caps repeat counts, so by hand).
func claudeTokenShape(token string) bool {
	if len(token) < 16 || len(token) > 4096 {
		return false
	}
	for _, r := range token {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// claudeTokenLifetime: Claude long-lived tokens last one year; Warden records
// 364 days so it refuses the token before Anthropic does.
const claudeTokenLifetime = 364 * 24 * time.Hour

// loginClaude is scripts/warden-claude-token in Go: the `claude setup-token`
// value is read at a hidden prompt (or from a pipe), validated and written
// as {"claudeAiOauth":{"accessToken":…,"expiresAt":…}}, mode 0600.
func (c *cli) loginClaude(authFile string, replace bool) error {
	if err := refuseExisting(authFile, replace); err != nil {
		return err
	}
	var token string
	var err error
	if c.terminal {
		fmt.Fprintf(c.stdout, "Run `claude setup-token` on a machine with a browser and paste its value here (not echoed).\n")
		token, err = readHidden(c.stdin, c.stdout, "Claude setup-token value: ")
	} else {
		token, err = readLine(c.stdin)
	}
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	doc, expires, err := claudeCredential(token, time.Now())
	if err != nil {
		return err
	}
	if err = atomicWrite(authFile, doc, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "wrote %s (mode 0600, uid %d, expires %s)\n", authFile, os.Getuid(), expires.UTC().Format("2006-01-02"))
	return nil
}

// claudeCredential validates a setup-token value and returns the file body.
func claudeCredential(token string, now time.Time) ([]byte, time.Time, error) {
	lower := strings.ToLower(token)
	if !claudeTokenShape(token) || strings.Contains(lower, "proxy") || strings.Contains(lower, "placeholder") {
		return nil, time.Time{}, errors.New("that is not a Claude long-lived token; run `claude setup-token` and paste its sk-ant-oat01-... value")
	}
	if !strings.HasPrefix(token, "sk-ant-oat") {
		return nil, time.Time{}, errors.New("expected a `claude setup-token` value (sk-ant-oat...), not an API key or session token")
	}
	expires := now.Add(claudeTokenLifetime)
	doc, err := json.MarshalIndent(map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "expiresAt": expires.UnixMilli()}}, "", "  ")
	if err != nil {
		return nil, time.Time{}, err
	}
	return append(doc, '\n'), expires, nil
}

// loginGitHub runs GitHub's device flow through chat/internal/login (Track
// D) or stores a pasted token, each writing the one 0600 file.
func (c *cli) loginGitHub(authFile, pasted string, pasteStdin, replace bool) error {
	if err := refuseExisting(authFile, replace); err != nil {
		return err
	}
	if pasteStdin {
		line, err := readLine(c.stdin)
		if err != nil {
			return err
		}
		pasted = strings.TrimSpace(line)
	}
	if pasted != "" {
		if err := githubLogin.Paste(pasted, authFile); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "wrote %s (mode 0600)\n", authFile)
		return nil
	}
	if release.GitHubOAuthClientID == "" {
		return errors.New("this Warden release has no GitHub OAuth client ID yet; store a token with `warden login github --paste-stdin` (for example from `gh auth token`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	login.OpenBrowser = openBrowser
	login.CopyToClipboard = copyToClipboard
	if err := githubLogin.Device(ctx, c.stdout, authFile); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "wrote %s (mode 0600)\n", authFile)
	return nil
}

// readLine reads one line from r.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readHidden prompts on the terminal and reads a line with echo off, using
// stty so no non-standard terminal package is needed.
func readHidden(in io.Reader, out io.Writer, prompt string) (string, error) {
	f, ok := in.(*os.File)
	if !ok {
		return readLine(in)
	}
	fmt.Fprint(out, prompt)
	if err := stty(f, "-echo"); err != nil {
		return "", fmt.Errorf("cannot hide the terminal input: %w", err)
	}
	line, err := readLine(f)
	stty(f, "echo")
	fmt.Fprintln(out)
	return line, err
}

func stty(tty *os.File, mode string) error {
	cmd := exec.Command("stty", mode)
	cmd.Stdin = tty
	cmd.Stderr = io.Discard
	return cmd.Run()
}
