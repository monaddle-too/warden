package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"warden/chat/internal/config"
)

// `warden instance` makes several Wardens on one machine legible
// (docs/host-dogfood-plan.md, Part A): list them, create one beside the
// default instance from the default instance's sign-ins, runtimes and
// guest image, remove one. An instance is a state directory with a name
// (state.go: instanceDir, instanceName).

const instanceUsage = `usage: warden instance COMMAND [flags]

  list    [--json]                 every instance on this machine: name, state directory,
                                   the release it runs from, chat and edge ports, service, dev
  create  NAME [--dev] [--from SOURCE] [--sbx PATH] [--service] [--menu]
                                   install into ~/.warden-NAME, then copy the provider sign-ins,
                                   runtimes and guest image pin from SOURCE (default: default),
                                   sharing SOURCE's SBX namespace and daemon; --dev marks a
                                   development instance (other builds install without --upgrade);
                                   --service / --menu register it with launchd like warden install
  rm      NAME [--yes]             stop it, unregister its service and menu bar item, remove its
                                   sandboxes and delete its state directory (never the default)
`

func (c *cli) instanceCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, instanceUsage)
		return errUsage
	}
	switch args[0] {
	case "list":
		return c.instanceList(args[1:])
	case "create":
		return c.instanceCreate(args[1:])
	case "rm", "remove":
		return c.instanceRemove(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, instanceUsage)
		return nil
	default:
		fmt.Fprintf(c.stderr, "warden instance: unknown command %q\n%s", args[0], instanceUsage)
		return errUsage
	}
}

// instanceInfo is one row of `warden instance list`.
type instanceInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// Release is the version of the release <state>/release links to, "-"
	// when there is no link; Installed the build that wrote install.json.
	Release   string `json:"release"`
	Installed string `json:"installed,omitempty"`
	ChatPort  int    `json:"chatPort,omitempty"`
	EdgePort  int    `json:"edgePort,omitempty"`
	// Service is the instance's background shape: running (pid N),
	// stopped, not registered, none (no service manager), or running
	// detached (pid N).
	Service string `json:"service"`
	Label   string `json:"label,omitempty"` // the service's launchd label / unit name
	Dev     bool   `json:"dev"`
	// SharedNamespace is the SBX namespace the instance borrows from
	// another instance, "" when it has its own.
	SharedNamespace string `json:"sharedNamespace,omitempty"`
	// Sandboxes counts the instance's sandboxes in the namespace when the
	// daemon answers (-1 when it does not).
	Sandboxes int `json:"sandboxes"`
	// Running is the version the instance runs now (running.json with a
	// live pid), "" when it is not running; RunningBinary that launcher,
	// PID its process, StartedAt when it came up. A running instance
	// whose launcher predates running.json shows "running" as its
	// version. PID is also filled for a service or a detached Warden
	// known only by its pid.
	Running       string    `json:"running,omitempty"`
	RunningBinary string    `json:"runningBinary,omitempty"`
	PID           int       `json:"pid,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	// Shape is the background shape in a word: registered running,
	// registered stopped, detached, foreground, not registered, none.
	Shape string `json:"shape"`
}

// instanceDirs lists every state directory that is an instance: the
// default one and its ~/.warden-* siblings, those with an install.json.
func instanceDirs() ([]string, error) {
	def, err := defaultStateDir()
	if err != nil {
		return nil, err
	}
	var dirs []string
	if _, err := os.Stat(filepath.Join(def, installFile)); err == nil {
		dirs = append(dirs, def)
	}
	entries, _ := os.ReadDir(filepath.Dir(def))
	var named []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".warden-") || !config.ValidInstanceName(strings.TrimPrefix(e.Name(), ".warden-")) {
			continue
		}
		path := filepath.Join(filepath.Dir(def), e.Name())
		if _, err := os.Stat(filepath.Join(path, installFile)); err == nil {
			named = append(named, path)
		}
	}
	sort.Strings(named)
	return append(dirs, named...), nil
}

// describeInstance reads what list shows about one state directory.
func (c *cli) describeInstance(state string) instanceInfo {
	info := instanceInfo{Name: instanceName(state), State: state, Release: "-", Sandboxes: -1}
	if record, found, err := readRecord(state); found && err == nil {
		info.Dev = record.Dev
		info.Installed = record.Warden
		if record.Name != "" {
			info.Name = record.Name
		}
	}
	if target, err := os.Readlink(filepath.Join(state, "release")); err == nil {
		if version, _, _, ok := parseReleaseName(filepath.Base(target)); ok {
			info.Release = version
		} else {
			info.Release = filepath.Base(target)
		}
	}
	cfg, err := config.Load(defaultConfigPath(state), state)
	if err != nil {
		info.ChatPort, info.EdgePort = rawPorts(defaultConfigPath(state))
	} else {
		info.ChatPort, info.EdgePort = portOf(cfg.Chat.Listen), portOf(cfg.Previews.EdgeListen)
		if sharedNamespace(cfg) {
			info.SharedNamespace = cfg.SBX.PrivateHome
		}
		if pid, alive := runningPID(cfg); alive {
			info.Service = fmt.Sprintf("running detached (pid %d)", pid)
			info.PID = pid
		}
	}
	svc, _ := c.service(state)
	var st serviceStatus
	if svc != nil && svc.registered() {
		st = svc.status()
	}
	switch {
	case info.Service != "":
		info.Shape = "detached"
	case svc == nil:
		info.Service, info.Shape = "none", "none"
	case !svc.registered():
		info.Service, info.Shape = "not registered", "not registered"
	case st.Running:
		info.Service, info.Shape = st.String(), "registered running"
		info.PID = st.PID
	default:
		info.Service, info.Shape = "stopped", "registered stopped"
	}
	if svc != nil {
		info.Label = svc.label()
	}
	if r, alive, _ := readRunning(state); alive {
		info.Running, info.RunningBinary, info.PID, info.StartedAt = r.Version, r.Binary, r.PID, r.StartedAt
		if info.Shape == "not registered" || info.Shape == "none" || info.Shape == "registered stopped" {
			info.Shape = "foreground"
		}
	} else if info.PID != 0 {
		// A launcher from before running.json: running, version unknown.
		info.Running = "running"
	}
	return info
}

// rawPorts reads chat.listen and previews.edgeListen from a warden.json
// the config package refuses (an older layout, say), so list still shows
// the ports.
func rawPorts(path string) (chat, edge int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	var file struct {
		Chat     struct{ Listen string }
		Previews struct{ EdgeListen string }
	}
	if json.Unmarshal(raw, &file) != nil {
		return 0, 0
	}
	return portOf(file.Chat.Listen), portOf(file.Previews.EdgeListen)
}

// otherInstancePorts are the chat and edge ports every other instance on
// this machine has in its warden.json, running or not, so a fresh install
// never takes them.
func (c *cli) otherInstancePorts(state string) []int {
	infos, err := c.instances()
	if err != nil {
		return nil
	}
	var ports []int
	for _, i := range infos {
		if filepath.Clean(i.State) == filepath.Clean(state) {
			continue
		}
		for _, p := range []int{i.ChatPort, i.EdgePort} {
			if p != 0 {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

// instances describes every instance on this machine.
func (c *cli) instances() ([]instanceInfo, error) {
	dirs, err := instanceDirs()
	if err != nil {
		return nil, err
	}
	var infos []instanceInfo
	for _, dir := range dirs {
		infos = append(infos, c.describeInstance(dir))
	}
	return infos, nil
}

func (c *cli) instanceList(args []string) error {
	fs := flag.NewFlagSet("warden instance list", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	asJSON := fs.Bool("json", false, "print the list as JSON")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	infos, err := c.instances()
	if err != nil {
		return err
	}
	if *asJSON {
		if infos == nil {
			infos = []instanceInfo{}
		}
		b, err := json.MarshalIndent(infos, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, string(b))
		return nil
	}
	if len(infos) == 0 {
		fmt.Fprintln(c.stdout, "no instances: nothing installed (`warden install` installs the default one)")
		return nil
	}
	printInstances(c.stdout, infos)
	return nil
}

// printInstances writes the `instance list` table.
func printInstances(w io.Writer, infos []instanceInfo) {
	rows := [][]string{{"NAME", "STATE", "PINNED", "RUNNING", "CHAT", "EDGE", "SERVICE", "DEV"}}
	for _, i := range infos {
		dev := "no"
		if i.Dev {
			dev = "yes"
		}
		rows = append(rows, []string{i.Name, i.State, i.Release, dash(i.Running), port(i.ChatPort), port(i.EdgePort), i.Service, dev})
	}
	printTable(w, rows)
}

// printStatusTable writes the `warden status` table: what runs where.
func printStatusTable(w io.Writer, infos []instanceInfo, now time.Time) {
	rows := [][]string{{"NAME", "RUNNING", "PINNED", "PID", "CHAT", "EDGE", "SERVICE", "UP"}}
	for _, i := range infos {
		pid, up := "-", "-"
		if i.PID != 0 {
			pid = strconv.Itoa(i.PID)
		}
		if !i.StartedAt.IsZero() && i.Running != "" {
			up = uptime(now.Sub(i.StartedAt))
		}
		rows = append(rows, []string{i.Name, dash(i.Running), i.Release, pid, port(i.ChatPort), port(i.EdgePort), i.Shape, up})
	}
	printTable(w, rows)
}

// uptime is a duration in the largest two units that matter.
func uptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// printTable writes rows (the first is the header) in aligned columns.
func printTable(w io.Writer, rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for _, r := range rows {
		var line strings.Builder
		for i, cell := range r {
			if i > 0 {
				line.WriteString("  ")
			}
			if i == len(r)-1 {
				line.WriteString(cell)
			} else {
				line.WriteString(cell + strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
}

func port(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// instanceCreate installs a fresh instance beside the source one and
// copies from the source what a device flow or a download would otherwise
// have to produce again: the provider sign-ins, the runtimes and the
// guest image pin. The new instance shares the source's SBX namespace and
// daemon (decision 3), so no second daemon starts and no keychain link is
// made; its sandboxes carry its name (sandboxes.namePrefix).
func (c *cli) instanceCreate(args []string) error {
	fs := flag.NewFlagSet("warden instance create", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, instanceUsage) }
	dev := fs.Bool("dev", false, "mark this a development instance: builds with other pins are installed without --upgrade")
	from := fs.String("from", defaultInstance, "the instance to copy the sign-ins, runtimes and guest image pin from, whose SBX namespace this one shares")
	sbxFlag := fs.String("sbx", "", "the sbx executable (default: the source instance's)")
	service := fs.Bool("service", false, "register the new instance with launchd / systemd --user and start it (as warden install does)")
	menu := fs.Bool("menu", false, "register the new instance's menu bar item (macOS)")
	if err := fs.Parse(interleaved(args, map[string]bool{"from": true, "sbx": true})); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprint(c.stderr, instanceUsage)
		return errUsage
	}
	name := fs.Arg(0)
	if name == defaultInstance {
		return errors.New("the default instance is installed by `warden install`, not created")
	}
	dir, err := instanceDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s exists; `warden instance rm %s` first, or choose another name", dir, name)
	}
	source, err := instanceDir(*from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	if _, err := os.Stat(filepath.Join(source, installFile)); err != nil {
		return fmt.Errorf("source instance %s (%s) is not installed: %w", *from, source, err)
	}
	srcCfg, err := loadSourceConfig(defaultConfigPath(source), source)
	if err != nil {
		return fmt.Errorf("source instance %s: %w", *from, err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Creating instance %s at %s from %s (%s)\n", name, dir, *from, source)
	for _, sub := range []string{"provider", "runtimes"} {
		src := filepath.Join(source, sub)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		n, err := copyTree(src, filepath.Join(dir, sub))
		if err != nil {
			return fmt.Errorf("copying %s: %w", src, err)
		}
		fmt.Fprintf(c.stdout, "%-16s %d file(s) copied from %s\n", sub+":", n, src)
	}
	sbxExe := *sbxFlag
	if sbxExe == "" {
		sbxExe = srcCfg.SBX.Executable
	}
	bugReports := "no"
	if srcCfg.Reporting.Enabled {
		bugReports = "yes"
	}
	in := c.newInstaller()
	in.state, in.configPath, in.sbxFlag = dir, defaultConfigPath(dir), sbxExe
	in.dev, in.service, in.menu, in.bugReports = *dev, *service, *menu, bugReports
	in.sharedHome, in.seed = srcCfg.SBX.PrivateHome, &srcCfg
	if err := in.execute(); err != nil {
		return fmt.Errorf("%w\n(the new instance at %s is incomplete; remove it and try again)", err, dir)
	}
	fmt.Fprintf(c.stdout, "\nInstance %s created. `warden start --instance %s --detach` (or `warden release install … --instance %s --restart`) runs it; `warden instance rm %s` removes it.\n", name, name, name, name)
	return nil
}

// loadSourceConfig reads what create copies from the source instance's
// warden.json. The source may run another release whose configuration
// this launcher does not fully know (a section it has not heard of), so
// when the config package refuses the file the few fields needed (the
// sbx section, the bug-reporting answer) are read raw over the defaults.
func loadSourceConfig(path, state string) (config.Config, error) {
	cfg, err := config.Load(path, state)
	if err == nil {
		return cfg, nil
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return cfg, err
	}
	var file struct {
		SBX       config.SBX `json:"sbx"`
		Reporting struct {
			Enabled bool `json:"enabled"`
		} `json:"reporting"`
	}
	if json.Unmarshal(raw, &file) != nil || file.SBX.PrivateHome == "" {
		return cfg, err
	}
	cfg = config.Defaults(state)
	cfg.SBX.Executable, cfg.SBX.PrivateHome = file.SBX.Executable, file.SBX.PrivateHome
	cfg.SBX.GuestImage, cfg.SBX.GuestImageDigest = file.SBX.GuestImage, file.SBX.GuestImageDigest
	cfg.Reporting.Enabled = file.Reporting.Enabled
	return cfg, nil
}

// newInstaller is an installer with the host facts filled in, ready for
// its state and flags.
func (c *cli) newInstaller() *installer {
	return &installer{c: c, client: &http.Client{}, memoryMB: hostMemoryMB(), cpus: runtime.NumCPU(), sbxLogin: true}
}

// copyTree copies the directory src to dst (which must not exist): with
// APFS clones on macOS (`cp -c`, no extra disk, seconds), a plain copy
// elsewhere or when that fails. Sockets, lock and pid files are skipped
// (what the clone script deleted). It counts the files copied.
func copyTree(src, dst string) (int, error) {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("cp", "-Rc", src, dst).CombinedOutput(); err == nil {
			return countFiles(dst), pruneCopied(dst)
		} else {
			_ = out
			os.RemoveAll(dst)
		}
	}
	n := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case skipCopied(d.Name(), info.Mode()):
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			n++
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil
		}
	})
	return n, err
}

// skipCopied is what a copy leaves out: sockets, and lock or pid files.
func skipCopied(name string, mode os.FileMode) bool {
	return mode&os.ModeSocket != 0 || mode&os.ModeNamedPipe != 0 || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".pid")
}

// pruneCopied removes from a cp -R copy what skipCopied leaves out.
func pruneCopied(dst string) error {
	return filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if skipCopied(d.Name(), info.Mode()) {
			return os.Remove(path)
		}
		return nil
	})
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// instanceRemove is uninstall for a named instance: never the default.
func (c *cli) instanceRemove(args []string) error {
	fs := flag.NewFlagSet("warden instance rm", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, instanceUsage) }
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(interleaved(args, nil)); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprint(c.stderr, instanceUsage)
		return errUsage
	}
	name := fs.Arg(0)
	dir, err := instanceDir(name)
	if err != nil {
		return err
	}
	if name == defaultInstance || instanceName(dir) == defaultInstance {
		return errors.New("the default instance is not removed this way; `warden uninstall` does that, on purpose")
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("instance %s: %w", name, err)
	}
	cfg, _, err := loadConfig("", dir)
	if err != nil {
		return err
	}
	if err := c.refuseForeground(cfg); err != nil {
		return err
	}
	if !*yes {
		if err := c.confirmRemoval(cfg, false); err != nil {
			return err
		}
	}
	return c.removeInstall(cfg, false)
}
