package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"warden/chat/internal/release"
)

// Runtime layout under <state>/runtimes:
//
//	codex/                 the extracted Codex bundle (runtimes.codex)
//	codex/.warden-sha256   the SHA-256 of the archive it came from
//	claude/claude          the Claude executable (runtimes.claude)
//
// Both are fetched by the URL and SHA-256 pinned in chat/internal/release for
// the guest architecture, exactly as deploy/guest/Dockerfile does.

const (
	codexDirName    = "codex"
	claudeDirName   = "claude"
	claudeFileName  = "claude"
	shaMarker       = ".warden-sha256"
	downloadTimeout = 30 * time.Minute
	downloadLimit   = 4 << 30 // no pinned runtime is anywhere near 4 GiB
)

// runtimeSources returns the pinned downloads for a guest architecture;
// tests replace it. An empty SHA means the release does not pin that
// architecture yet.
var runtimeSources = func(arch string) (codex, claude release.Runtime) {
	return release.CodexBundles[arch], release.ClaudeExecutables[arch]
}

func codexDir(runtimes string) string { return filepath.Join(runtimes, codexDirName) }
func claudePath(runtimes string) string {
	return filepath.Join(runtimes, claudeDirName, claudeFileName)
}

// download fetches url into dest (through a temporary file beside it),
// verifying the SHA-256 before the file appears at dest.
func download(ctx context.Context, client *http.Client, url, sha, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".download"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		f.Close()
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		f.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.Close()
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, downloadLimit+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > downloadLimit {
		return fmt.Errorf("download %s: larger than %d bytes", url, downloadLimit)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sha {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, release pins %s; the file was discarded", url, got, sha)
	}
	return os.Rename(tmp, dest)
}

// fileSHA256 returns the hex digest of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// extractTarGz unpacks archive into dir, which must not exist. Entries with
// absolute or parent-relative names, and links escaping dir, are refused.
func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()
	if err = os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	reader := tar.NewReader(gz)
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		target, err := safeJoin(dir, header.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += header.Size
			if total > downloadLimit {
				return fmt.Errorf("%s: archive expands beyond %d bytes", archive, downloadLimit)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0o777
			if mode&0o400 == 0 {
				mode |= 0o400
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, reader)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("%s: absolute symlink %s", archive, header.Name)
			}
			if _, err = safeJoin(dir, filepath.Join(filepath.Dir(header.Name), header.Linkname)); err != nil {
				return fmt.Errorf("%s: symlink %s escapes the bundle", archive, header.Name)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err = os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported entry type %d for %s", archive, header.Typeflag, header.Name)
		}
	}
}

// safeJoin joins an archive entry name under dir, refusing absolute names
// and any ".." component rather than normalising them away.
func safeJoin(dir, name string) (string, error) {
	slash := filepath.ToSlash(name)
	if strings.HasPrefix(slash, "/") {
		return "", fmt.Errorf("entry %q is absolute", name)
	}
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return "", fmt.Errorf("entry %q escapes the bundle", name)
		}
	}
	return filepath.Join(dir, filepath.FromSlash(slash)), nil
}

// codexBundle is codex-package.json; the field set the runner validates.
type codexBundle struct {
	Version       string `json:"version"`
	Target        string `json:"target"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
	LayoutVersion int    `json:"layoutVersion"`
}

// codexBundleExecutables are the files deploy/guest/Dockerfile requires to
// be executable after extraction.
var codexBundleExecutables = []string{"bin/codex", "bin/codex-code-mode-host", "codex-path/rg", "codex-resources/bwrap"}

// checkCodexBundle applies the runner's codexBundleMetadata rules for arch
// and the Dockerfile's executable checks, returning the first problem.
func checkCodexBundle(dir, arch string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "codex-package.json"))
	if err != nil {
		return errors.New("pinned Linux runtime bundle metadata is missing (" + filepath.Join(dir, "codex-package.json") + ")")
	}
	var metadata codexBundle
	if json.Unmarshal(raw, &metadata) != nil {
		return errors.New("invalid runtime bundle metadata in " + dir)
	}
	if metadata.Version != release.CodexVersion || metadata.Target != codexTarget(arch) || metadata.LayoutVersion != 1 || metadata.Entrypoint != "bin/codex" || metadata.ResourcesDir != "codex-resources" || metadata.PathDir != "codex-path" {
		return fmt.Errorf("runtime bundle must be Codex %s for %s with layout 1 (bin/codex, codex-resources, codex-path); %s describes Codex %s for %s layout %d", release.CodexVersion, codexTarget(arch), filepath.Join(dir, "codex-package.json"), metadata.Version, metadata.Target, metadata.LayoutVersion)
	}
	for _, rel := range codexBundleExecutables {
		if err := executableFile(filepath.Join(dir, rel)); err != nil {
			return fmt.Errorf("%s is not an executable file in the bundle: %v", rel, err)
		}
	}
	return checkWorldReadable(dir)
}

// checkWorldReadable reports the first entry of a runtime tree that the
// guest's agent user could not read or traverse after `sbx cp`.
func checkWorldReadable(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm()
		if d.IsDir() && perm&0o005 != 0o005 {
			return fmt.Errorf("%s is mode %04o; the guest's agent user cannot traverse it (run `warden install` to fix the modes)", path, perm)
		}
		if !d.IsDir() && perm&0o004 == 0 {
			return fmt.Errorf("%s is mode %04o; the guest's agent user cannot read it (run `warden install` to fix the modes)", path, perm)
		}
		return nil
	})
}

// checkClaude verifies the Claude executable against the pinned SHA-256.
func checkClaude(path, sha string) error {
	if err := executableFile(path); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if sha == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("%s has SHA-256 %s, release pins %s for Claude %s", path, got, sha, release.ClaudeVersion)
	}
	return nil
}

// runtimeChecks are the doctor lines for the installed runtimes.
func runtimeChecks(codex, claude, arch string) []check {
	var out []check
	codexSrc, claudeSrc := runtimeSources(arch)
	if codex == "" || claude == "" {
		return []check{fail("runtimes", "warden.json names no runtimes.codex / runtimes.claude", "run `warden install` to fetch Codex "+release.CodexVersion+" and Claude Code "+release.ClaudeVersion+" for "+arch)}
	}
	if err := checkCodexBundle(codex, arch); err != nil {
		out = append(out, fail("codex bundle", err.Error(), "run `warden install` to fetch Codex "+release.CodexVersion+" for "+arch))
	} else {
		detail := "Codex " + release.CodexVersion + " " + codexTarget(arch) + " at " + codex
		if marker, err := os.ReadFile(filepath.Join(codex, shaMarker)); err == nil && codexSrc.SHA256 != "" && strings.TrimSpace(string(marker)) != codexSrc.SHA256 {
			out = append(out, fail("codex bundle", detail+" came from an archive with SHA-256 "+strings.TrimSpace(string(marker))+", release pins "+codexSrc.SHA256, "run `warden install` to fetch the pinned archive"))
		} else {
			out = append(out, pass("codex bundle", detail))
		}
	}
	if err := checkClaude(claude, claudeSrc.SHA256); err != nil {
		out = append(out, fail("claude executable", err.Error(), "run `warden install` to fetch Claude Code "+release.ClaudeVersion+" for "+arch))
	} else {
		out = append(out, pass("claude executable", "Claude Code "+release.ClaudeVersion+" at "+claude))
	}
	return out
}

// worldReadable makes a runtime tree readable and traversable by everyone
// (directories 0755, files a+r with their execute bits kept). The runner
// copies the tree into guests with `sbx cp`, which preserves modes and the
// host uid, so an owner-only tree is unreadable for the guest's agent user
// and the agent cannot start ("Permission denied"). The OVH copy is
// root-owned and world-readable for the same reason.
func worldReadable(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := info.Mode().Perm()
		if d.IsDir() {
			mode = 0o755
		} else {
			mode |= 0o444
			if mode&0o100 != 0 {
				mode |= 0o111
			}
		}
		if mode != info.Mode().Perm() {
			return os.Chmod(path, mode)
		}
		return nil
	})
}
