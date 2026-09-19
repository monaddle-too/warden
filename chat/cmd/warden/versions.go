package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// `warden versions` lists what can be installed and run on this machine:
// the GitHub releases of monaddle-too/warden that `warden release install
// TAG` can download, and the release store (with which instances are
// pinned to and running each version) (docs/host-dogfood-plan.md, Part
// C). Offline, the GitHub half is one line and the store is listed.

const versionsUsage = `usage: warden versions [--json]

  lists the GitHub releases of monaddle-too/warden (TAG, DATE, whether a tarball
  for this host exists, whether the store has it; 10 s, offline: one line), then
  the releases in the store (~/.warden/releases) and the instances' older copies:
  VERSION, INSTALLED, PINNED BY (the instances whose release link points at it),
  RUNNING ON (the instances running it now); * marks this launcher's own version.
`

// githubReleasesURL is the public releases API of the Warden repository;
// tests point it at a local server.
var githubReleasesURL = "https://api.github.com/repos/monaddle-too/warden/releases"

// githubTimeout bounds every request to GitHub.
const githubTimeout = 10 * time.Second

// githubRelease is what the API says about one release.
type githubRelease struct {
	Tag         string        `json:"tag_name"`
	Name        string        `json:"name"`
	PublishedAt time.Time     `json:"published_at"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// asset finds one by name.
func (r githubRelease) asset(name string) *githubAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// hostTarball is the tarball name a release carries for this host.
func hostTarball(tag string) string {
	return fmt.Sprintf("warden-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// fetchGitHubReleases lists the repository's releases, drafts left out.
func fetchGitHubReleases(ctx context.Context) ([]githubRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesURL+"?per_page=50", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "warden/"+revision)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", githubReleasesURL, resp.Status)
	}
	var all []githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&all); err != nil {
		return nil, fmt.Errorf("%s: %w", githubReleasesURL, err)
	}
	out := all[:0]
	for _, r := range all {
		if !r.Draft {
			out = append(out, r)
		}
	}
	return out, nil
}

// versionRow is one line of `warden versions`.
type versionRow struct {
	Version   string    `json:"version"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Installed time.Time `json:"installedAt"`
	Legacy    string    `json:"legacy,omitempty"`
	PinnedBy  []string  `json:"pinnedBy"`
	RunningOn []string  `json:"runningOn"`
	This      bool      `json:"this"` // this launcher's version
}

// remoteRow is one GitHub release in the listing.
type remoteRow struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"publishedAt"`
	Prerelease  bool      `json:"prerelease"`
	HostTarball bool      `json:"hostTarball"` // the release has a tarball for this os/arch
	Checksums   bool      `json:"checksums"`   // and a SHA256SUMS to check it against
	Installed   bool      `json:"installed"`   // the store has it
}

func (c *cli) versionRows() ([]versionRow, error) {
	infos, err := c.instances()
	if err != nil {
		return nil, err
	}
	var states []string
	for _, i := range infos {
		states = append(states, i.State)
	}
	releases, err := allReleases(states)
	if err != nil {
		return nil, err
	}
	users := c.releaseUsersOf(infos)
	rows := []versionRow{}
	for _, r := range releases {
		path := filepath.Clean(r.Path)
		row := versionRow{Version: r.Version, Name: r.Name, Path: r.Path, Installed: r.At, Legacy: r.Legacy, PinnedBy: users.pinned[path], RunningOn: users.running[path], This: r.Version == revision}
		if row.PinnedBy == nil {
			row.PinnedBy = []string{}
		}
		if row.RunningOn == nil {
			row.RunningOn = []string{}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (c *cli) versions(args []string) error {
	fs := flag.NewFlagSet("warden versions", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, versionsUsage) }
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprint(c.stderr, versionsUsage)
		return errUsage
	}
	rows, err := c.versionRows()
	if err != nil {
		return err
	}
	remotes, remoteErr := c.remoteRows(context.Background(), rows)
	if *asJSON {
		out := struct {
			Installed []versionRow `json:"installed"`
			Remote    []remoteRow  `json:"remote,omitempty"`
			RemoteErr string       `json:"remoteError,omitempty"`
		}{Installed: rows, Remote: remotes}
		if remoteErr != nil {
			out.RemoteErr = remoteErr.Error()
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, string(b))
		return nil
	}
	switch {
	case remoteErr != nil:
		fmt.Fprintf(c.stdout, "GitHub releases (monaddle-too/warden): unreachable: %v\n", remoteErr)
	case len(remotes) == 0:
		fmt.Fprintln(c.stdout, "GitHub releases (monaddle-too/warden): none published")
	default:
		fmt.Fprintln(c.stdout, "GitHub releases (monaddle-too/warden):")
		table := [][]string{{"TAG", "DATE", "FOR THIS HOST", "STORE"}}
		for _, r := range remotes {
			host := "no tarball for " + runtime.GOOS + "/" + runtime.GOARCH
			switch {
			case r.HostTarball && r.Checksums:
				host = "yes"
			case r.HostTarball:
				host = "yes (no SHA256SUMS: not installable)"
			}
			tag := r.Tag
			if r.Prerelease {
				tag += " (pre-release)"
			}
			state := "-"
			if r.Installed {
				state = "installed"
			}
			table = append(table, []string{tag, r.PublishedAt.UTC().Format("2006-01-02"), host, state})
		}
		printTable(c.stdout, table)
		fmt.Fprintln(c.stdout, "(`warden release install TAG --instance NAME` downloads one into the store)")
	}
	fmt.Fprintln(c.stdout)
	store, _ := storeDir()
	if len(rows) == 0 {
		fmt.Fprintf(c.stdout, "no releases in %s (`warden release install TARBALL|TAG` or `warden release build` adds one)\n", store)
		return nil
	}
	fmt.Fprintf(c.stdout, "Installed (%s):\n", store)
	table := [][]string{{"", "VERSION", "INSTALLED", "PINNED BY", "RUNNING ON", "WHERE"}}
	for _, r := range rows {
		mark := " "
		if r.This {
			mark = "*"
		}
		where := "store"
		if r.Legacy != "" {
			where = r.Legacy + " (older copy)"
		}
		table = append(table, []string{mark, r.Version, r.Installed.Local().Format("2006-01-02 15:04"), dash(strings.Join(r.PinnedBy, ",")), dash(strings.Join(r.RunningOn, ",")), where})
	}
	printTable(c.stdout, table)
	fmt.Fprintf(c.stdout, "(* this launcher, %s)\n", revision)
	return nil
}

// remoteRows lists GitHub's releases against the store.
func (c *cli) remoteRows(ctx context.Context, installed []versionRow) ([]remoteRow, error) {
	releases, err := fetchGitHubReleases(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range installed {
		have[r.Version] = true
	}
	rows := []remoteRow{}
	for _, r := range releases {
		rows = append(rows, remoteRow{Tag: r.Tag, PublishedAt: r.PublishedAt, Prerelease: r.Prerelease, HostTarball: r.asset(hostTarball(r.Tag)) != nil, Checksums: r.asset(checksumsAsset) != nil, Installed: have[r.Tag]})
	}
	return rows, nil
}

// checksumsAsset is the file scripts/release.sh writes beside the
// tarballs and the release workflow attaches.
const checksumsAsset = "SHA256SUMS"

// placeRemoteRelease downloads the GitHub release tag's tarball for this
// host into the store, checked against the release's SHA256SUMS, and
// unpacks it. A release without a SHA256SUMS is refused.
func (c *cli) placeRemoteRelease(store, tag string, force bool) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	dest := filepath.Join(store, strings.TrimSuffix(hostTarball(tag), ".tar.gz"))
	if !force {
		if err := executableFile(filepath.Join(dest, "bin", "warden")); err == nil {
			fmt.Fprintf(c.stdout, "release:         %s is already in the store (--force downloads it again)\n", filepath.Base(dest))
			return dest, nil
		}
	}
	ctx := context.Background()
	releases, err := fetchGitHubReleases(ctx)
	if err != nil {
		return "", fmt.Errorf("%s is neither a file nor a reachable GitHub release: %w", tag, err)
	}
	var found *githubRelease
	for i := range releases {
		if releases[i].Tag == tag {
			found = &releases[i]
		}
	}
	if found == nil {
		return "", fmt.Errorf("%s is neither a file nor a GitHub release of monaddle-too/warden (`warden versions` lists them)", tag)
	}
	tarball := found.asset(hostTarball(found.Tag))
	if tarball == nil {
		return "", fmt.Errorf("release %s has no tarball for %s/%s", found.Tag, runtime.GOOS, runtime.GOARCH)
	}
	sums := found.asset(checksumsAsset)
	if sums == nil {
		return "", fmt.Errorf("release %s has no %s to check %s against; refusing to install an unverified download (download it yourself and `warden release install PATH`)", found.Tag, checksumsAsset, tarball.Name)
	}
	if err := ensurePrivateDir(store); err != nil {
		return "", err
	}
	fmt.Fprintf(c.stdout, "release:         downloading %s (%s)\n", tarball.Name, byteSize(tarball.Size))
	sumsText, err := fetchBytes(ctx, sums.URL, 1<<20)
	if err != nil {
		return "", err
	}
	want, err := checksumFor(string(sumsText), tarball.Name)
	if err != nil {
		return "", fmt.Errorf("release %s: %w", found.Tag, err)
	}
	tmp := dest + ".download"
	os.Remove(tmp)
	defer os.Remove(tmp)
	if err := downloadTo(ctx, tarball.URL, tmp, want); err != nil {
		return "", fmt.Errorf("%s: %w", tarball.Name, err)
	}
	if err := unpackRelease(tmp, dest); err != nil {
		return "", err
	}
	fmt.Fprintf(c.stdout, "release:         unpacked %s (%s, SHA256 verified) to %s\n", tarball.Name, found.Tag, dest)
	return dest, nil
}

// checksumFor finds name's digest in a sha256sum listing.
func checksumFor(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no checksum for %s", checksumsAsset, name)
}

func fetchBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "warden/"+revision)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// downloadTo streams url to path and refuses a digest other than want
// (the file is removed then). A download is bounded by time per read,
// not in all: a tarball is tens of megabytes.
func downloadTo(ctx context.Context, url, path, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "warden/"+revision)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(path)
		return fmt.Errorf("SHA256 %s does not match %s in %s; the download is discarded", got, want, checksumsAsset)
	}
	return nil
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var errNoVersion = errors.New("warden start --version needs a version (`warden versions` lists them)")
