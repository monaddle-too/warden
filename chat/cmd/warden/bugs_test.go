package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/bugreport"
	"warden/chat/internal/config"
)

func reportingOf(t *testing.T, path string) config.Reporting {
	t.Helper()
	cfg, err := config.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Reporting
}

// The install question: answered by the flag, by the terminal, kept on a
// re-run, and defaulted to no when nobody can be asked.
func TestInstallAsksAboutBugReportsOnce(t *testing.T) {
	f := newFixture(t)
	code, out := f.install("--bug-reports=no")
	if code != 0 {
		t.Fatalf("install (%d):\n%s", code, out)
	}
	configPath := filepath.Join(f.state, "warden.json")
	if strings.Contains(out, bugReportsQuestion) || !strings.Contains(out, "bug reports:     off") || !strings.Contains(out, "Installed. Bug reports: off") {
		t.Fatalf("output:\n%s", out)
	}
	if r := reportingOf(t, configPath); r.Enabled || r.URL != config.DefaultReportingURL {
		t.Fatalf("%+v", r)
	}
	// A re-run without the flag keeps the answer and does not ask.
	if code, out = f.install(); code != 0 || strings.Contains(out, bugReportsQuestion) || !strings.Contains(out, "bug reports:     off") {
		t.Fatalf("re-run (%d):\n%s", code, out)
	}
	// The flag changes it.
	if code, out = f.install("--bug-reports=yes"); code != 0 || !strings.Contains(out, "bug reports:     on") {
		t.Fatalf("yes (%d):\n%s", code, out)
	}
	if !reportingOf(t, configPath).Enabled {
		t.Fatal("not enabled")
	}
	if code, out = f.install(); code != 0 || !strings.Contains(out, "bug reports:     on") || !strings.Contains(out, "Installed. Bug reports: on") {
		t.Fatalf("re-run after yes (%d):\n%s", code, out)
	}
	if code, out = f.install("--bug-reports=maybe"); code != 2 || !strings.Contains(out, "--bug-reports must be yes or no") {
		t.Fatalf("bad flag (%d):\n%s", code, out)
	}
	// A fresh state on a terminal asks; "y" turns it on.
	g := newFixture(t)
	code, out = g.run("y\n", "install", "--state", g.state, "--sbx", g.sbx)
	if code != 0 || !strings.Contains(out, bugReportsQuestion) || !strings.Contains(out, "bug reports:     on") {
		t.Fatalf("asked (%d):\n%s", code, out)
	}
	if !reportingOf(t, filepath.Join(g.state, "warden.json")).Enabled {
		t.Fatal("y did not enable")
	}
	// Enter alone, or a closed stdin, is the default: no.
	h := newFixture(t)
	if code, out = h.run("\n", "install", "--state", h.state, "--sbx", h.sbx); code != 0 || !strings.Contains(out, "bug reports:     off") {
		t.Fatalf("enter (%d):\n%s", code, out)
	}
	// Without a terminal the question is not asked; the setting is off with
	// the way to change it.
	k := newFixture(t)
	c := &cli{stdin: strings.NewReader(""), stdout: &k.out, stderr: &k.out, terminal: false}
	code = c.run([]string{"install", "--state", k.state, "--sbx", k.sbx, "--sbx-login=false"})
	out = k.out.String()
	if strings.Contains(out, bugReportsQuestion) || !strings.Contains(out, "no terminal to ask") {
		t.Fatalf("no terminal (%d):\n%s", code, out)
	}
}

// A failed step, with reporting on, drafts an install-step report carrying
// the step, the error and the installer's output, and presents it before
// the installer exits; with reporting off nothing is written.
func TestInstallFailureDraftsAndPresentsAReport(t *testing.T) {
	presentTimeout = 5 * time.Second
	t.Cleanup(func() { presentTimeout = bugreport.DefaultTimeout })
	f := newFixture(t)
	f.set("mcp", `{"name":"filesystem","transport":"stdio"}`) // the host checks fail
	opened := make(chan string, 1)
	c := &cli{stdin: strings.NewReader(""), stdout: &f.out, stderr: &f.out, terminal: true, openFn: func(u string) error { opened <- u; return nil }}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case u := <-opened:
			res, err := http.Post(u+"/discard", "", nil)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
		case <-time.After(5 * time.Second):
			t.Error("the page was not opened")
		}
	}()
	code := c.run([]string{"install", "--state", f.state, "--sbx", f.sbx, "--bug-reports=yes"})
	wg.Wait()
	out := f.out.String()
	if code == 0 {
		t.Fatalf("install passed:\n%s", out)
	}
	if !strings.Contains(out, "Bug report ") || !strings.Contains(out, "review and send or discard it at") || !strings.Contains(out, "bug report discarded") {
		t.Fatalf("output:\n%s", out)
	}
	// The draft was discarded from the page; nothing is pending.
	if drafts, _ := bugreport.Pending(f.state); len(drafts) != 0 {
		t.Fatalf("pending: %+v", drafts)
	}
	// Without the page: the draft's content.
	g := newFixture(t)
	g.set("mcp", `{"name":"filesystem","transport":"stdio"}`)
	g.set("login", "") // signed in already, so the host checks are reached without a terminal
	c = &cli{stdin: strings.NewReader(""), stdout: &g.out, stderr: &g.out, terminal: false}
	if code = c.run([]string{"install", "--state", g.state, "--sbx", g.sbx, "--bug-reports=yes"}); code == 0 {
		t.Fatal("passed")
	}
	if !strings.Contains(g.out.String(), "drafted; `warden bugs pending` shows it for review") {
		t.Fatalf("output:\n%s", g.out.String())
	}
	drafts, _ := bugreport.Pending(g.state)
	if len(drafts) != 1 {
		t.Fatalf("pending: %+v", drafts)
	}
	r := drafts[0].Report
	if r.Kind != bugreport.KindError || r.Trigger != bugreport.TriggerInstallStep || r.Component != bugreport.ComponentInstall || r.Error == nil || r.Error.Operation != "host checks" || !strings.Contains(r.Error.Message, "does not satisfy the policy verifier") || !strings.HasPrefix(r.Summary, "warden install failed at host checks: ") {
		t.Fatalf("%+v", r)
	}
	if len(r.Logs) != 1 || r.Logs[0].Name != "warden install output" || !strings.Contains(strings.Join(r.Logs[0].Lines, "\n"), "FAIL sbx mcp inventory") {
		t.Fatalf("%+v", r.Logs)
	}
	// Off: the same failure writes nothing.
	h := newFixture(t)
	h.set("mcp", `{"name":"filesystem","transport":"stdio"}`)
	if code, _ := h.install("--bug-reports=no"); code == 0 {
		t.Fatal("passed")
	}
	if _, err := os.Stat(bugreport.PendingDir(h.state)); err == nil {
		t.Fatal("a draft was written with reporting off")
	}
}

func TestBugsStatusOnOffAndPending(t *testing.T) {
	state, configPath := loginFixture(t)
	code, out := runCLI("", false, "bugs", "status", "--config", configPath)
	if code != 0 || !strings.Contains(out, "bug reports: off ("+config.DefaultReportingURL+")") || !strings.Contains(out, "pending:     none") {
		t.Fatalf("(%d) %s", code, out)
	}
	if code, out = runCLI("", false, "bugs", "on", "--config", configPath); code != 0 || !strings.Contains(out, "bug reports: on") {
		t.Fatalf("(%d) %s", code, out)
	}
	if !reportingOf(t, configPath).Enabled {
		t.Fatal("on did not write")
	}
	// The rest of the file is kept as install wrote it.
	raw, _ := os.ReadFile(configPath)
	if !strings.Contains(string(raw), `"state": "`+state+`"`) || !strings.Contains(string(raw), `"enabled": true`) {
		t.Fatalf("%s", raw)
	}
	if code, out = runCLI("", false, "bugs", "off", "--config", configPath); code != 0 || !strings.Contains(out, "bug reports: off") {
		t.Fatalf("(%d) %s", code, out)
	}
	if reportingOf(t, configPath).Enabled {
		t.Fatal("off did not write")
	}
	if code, out = runCLI("", false, "bugs", "pending", "--config", configPath); code != 0 || !strings.Contains(out, "no pending bug reports") {
		t.Fatalf("(%d) %s", code, out)
	}
	if code, out = runCLI("", false, "bugs", "--config", configPath); code != 2 || !strings.Contains(out, "usage: warden bugs") {
		t.Fatalf("(%d) %s", code, out)
	}
	if code, out = runCLI("", false, "bugs", "frobnicate", "--config", configPath); code != 2 || !strings.Contains(out, "unknown command") {
		t.Fatalf("(%d) %s", code, out)
	}
	// send and test refuse while reporting is off, writing nothing.
	if code, out = runCLI("", false, "bugs", "send", "--config", configPath, "it broke"); code != 1 || !strings.Contains(out, bugreport.OffNotice) {
		t.Fatalf("(%d) %s", code, out)
	}
	if code, out = runCLI("", false, "bugs", "test", "--config", configPath); code != 1 || !strings.Contains(out, bugreport.OffNotice) {
		t.Fatalf("(%d) %s", code, out)
	}
	if code, out = runCLI("", false, "bugs", "send", "--config", configPath); code != 2 || !strings.Contains(out, "text is required") {
		t.Fatalf("(%d) %s", code, out)
	}
	if _, err := os.Stat(bugreport.PendingDir(state)); err == nil {
		t.Fatal("drafted while off")
	}
}

// send and test with reporting on and no Warden running: the draft is
// presented in this process; pending lists and re-offers it.
func TestBugsSendTestAndPendingPresentInProcess(t *testing.T) {
	presentTimeout = 5 * time.Second
	t.Cleanup(func() { presentTimeout = bugreport.DefaultTimeout })
	state, configPath := loginFixture(t)
	os.WriteFile(filepath.Join(state, "warden.log"), []byte("launcher line\n"), 0o600)
	os.WriteFile(filepath.Join(state, "warden-chat.log"), []byte("chat line for someone@example.com\n"), 0o600)
	runCLI("", false, "bugs", "on", "--config", configPath)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep bugreport.Report
		json.NewDecoder(r.Body).Decode(&rep)
		w.WriteHeader(202)
		w.Write([]byte(`{"id":"` + rep.ID + `"}`))
	}))
	defer receiver.Close()
	cfg, _ := config.Load(configPath, "")
	cfg.Reporting.URL = receiver.URL
	config.Write(configPath, cfg)
	// A decision made on the page by a driver: discard, or send.
	drive := func(decision string) (*cli, *strings.Builder, func()) {
		var out strings.Builder
		opened := make(chan string, 1)
		done := make(chan struct{})
		c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &out, terminal: false, openFn: func(u string) error { opened <- u; return nil }}
		go func() {
			defer close(done)
			select {
			case u := <-opened:
				res, err := http.Post(u+"/"+decision, "application/x-www-form-urlencoded", strings.NewReader("description=it+broke"))
				if err != nil {
					t.Error(err)
					return
				}
				res.Body.Close()
			case <-time.After(5 * time.Second):
				t.Error("no page")
			}
		}()
		return c, &out, func() { <-done }
	}
	c, out, wait := drive("discard")
	code := c.run([]string{"bugs", "send", "--config", configPath, "the", "spinner", "never", "stops"})
	wait()
	if code != 0 || !strings.Contains(out.String(), "bug report discarded") {
		t.Fatalf("(%d) %s", code, out.String())
	}
	if drafts, _ := bugreport.Pending(state); len(drafts) != 0 {
		t.Fatalf("%+v", drafts)
	}
	// test without a running chat: raised here, drafted as the CLI's.
	c, out, wait = drive("send")
	code = c.run([]string{"bugs", "test", "--config", configPath})
	wait()
	if code != 0 || !strings.Contains(out.String(), "raised in this process") || !strings.Contains(out.String(), "bug report sent; its id is ") {
		t.Fatalf("(%d) %s", code, out.String())
	}
	// A draft nobody decided on: listed, then offered again.
	cap := bugreport.New(cfg, configPath, bugreport.ComponentCLI)
	r := cap.Draft(bugreport.KindUser, bugreport.TriggerUser, "older report")
	r.Description = "older report"
	if _, written, err := cap.Capture(r); err != nil || !written {
		t.Fatal(written, err)
	}
	code, listed := runCLI("", false, "bugs", "pending", "--list", "--config", configPath)
	if code != 0 || !strings.Contains(listed, " 1  ") || !strings.Contains(listed, "user  user         cli       older report") {
		t.Fatalf("(%d) %s", code, listed)
	}
	if code, status := runCLI("", false, "bugs", "status", "--config", configPath); code != 0 || !strings.Contains(status, "pending:     1 draft") {
		t.Fatalf("(%d) %s", code, status)
	}
	c, out, wait = drive("discard")
	code = c.run([]string{"bugs", "pending", "--config", configPath})
	wait()
	if code != 0 || !strings.Contains(out.String(), "older report") || !strings.Contains(out.String(), "bug report discarded") {
		t.Fatalf("(%d) %s", code, out.String())
	}
	if drafts, _ := bugreport.Pending(state); len(drafts) != 0 {
		t.Fatalf("%+v", drafts)
	}
}

// The launcher's watch: drafts already pending at start are counted, not
// shown; every draft that appears later is presented exactly once, even
// when the page reached no decision.
func TestDraftWatcherPresentsEachNewDraftOnce(t *testing.T) {
	state, configPath := loginFixture(t)
	runCLI("", false, "bugs", "on", "--config", configPath)
	cfg, _ := config.Load(configPath, "")
	cap := bugreport.New(cfg, configPath, bugreport.ComponentChat)
	write := func(summary string) string {
		path, written, err := cap.Capture(cap.Draft(bugreport.KindError, bugreport.TriggerPanic, summary))
		if err != nil || !written {
			t.Fatal(written, err)
		}
		return path
	}
	old := write("before the watch")
	var mu sync.Mutex
	var shown []string
	var log strings.Builder
	w := &draftWatcher{state: state, interval: 10 * time.Millisecond, log: &log, present: func(ctx context.Context, path string) (bugreport.Outcome, error) {
		mu.Lock()
		shown = append(shown, path)
		mu.Unlock()
		return bugreport.Outcome{TimedOut: true}, nil // the draft stays
	}}
	if n := w.start(); n != 1 {
		t.Fatalf("%d waiting", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)
	one := write("first after")
	two := write("second after")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(shown)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a few more ticks: nothing is shown twice
	mu.Lock()
	got := append([]string(nil), shown...)
	mu.Unlock()
	if len(got) != 2 || got[0] == old || got[1] == old || (got[0] != one && got[0] != two) || got[0] == got[1] {
		t.Fatalf("shown %v (old %s)", got, old)
	}
	if !strings.Contains(log.String(), "stays pending") {
		t.Fatalf("log: %s", log.String())
	}
	// presentNew directly (the exit path) shows only what is new.
	three := write("third")
	w.presentNew(ctx)
	mu.Lock()
	last := shown[len(shown)-1]
	n := len(shown)
	mu.Unlock()
	if n != 3 || last != three {
		t.Fatalf("shown %v", shown)
	}
}

// The service-exit draft: the exit, the service's log tail and the
// launcher's; skipped when that service just drafted the panic that took
// it down; nothing when reporting is off.
func TestExitDraftCarriesTheLogsAndYieldsToAPanicDraft(t *testing.T) {
	state, configPath := loginFixture(t)
	cfg, _ := config.Load(configPath, "")
	cap := bugreport.New(cfg, "", bugreport.ComponentLauncher)
	os.WriteFile(filepath.Join(state, "warden-runner.log"), []byte("runner: boot\nrunner: fatal for someone@example.com\n"), 0o600)
	os.WriteFile(filepath.Join(state, "warden.log"), []byte("warden: started\n"), 0o600)
	if _, written, err := exitDraft(cap, "warden-runner", "exit status 2", time.Now()); err != nil || written {
		t.Fatal("drafted while off", written, err)
	}
	cfg.Reporting.Enabled = true
	cap = bugreport.New(cfg, "", bugreport.ComponentLauncher)
	path, written, err := exitDraft(cap, "warden-runner", "exit status 2", time.Now())
	if err != nil || !written {
		t.Fatal(written, err)
	}
	r, _ := bugreport.Read(path)
	if r.Trigger != bugreport.TriggerServiceExit || r.Component != bugreport.ComponentLauncher || r.Summary != "warden-runner exited unexpectedly (exit status 2)" || r.Error.Operation != "warden-runner" || len(r.Logs) != 2 || r.Logs[0].Name != "warden-runner.log" || r.Logs[0].Lines[1] != "runner: fatal for <email>" || r.Logs[1].Name != "warden.log" {
		t.Fatalf("%+v", r)
	}
	// A panic draft from the runner moments ago: the exit is not drafted.
	runner := bugreport.New(cfg, "", bugreport.ComponentRunner)
	if _, _, err := runner.CapturePanic(bugreport.TriggerPanic, "op prepare", "boom", []byte("stack")); err != nil {
		t.Fatal(err)
	}
	if _, written, _ := exitDraft(cap, "warden-runner", "exit status 2", time.Now()); written {
		t.Fatal("the exit was drafted beside the panic")
	}
	// Another service's panic does not cover the runner's exit.
	if _, written, _ := exitDraft(cap, "warden-chat", "signal: killed", time.Now()); !written {
		t.Fatal("the chat exit was not drafted")
	}
	// A process state reads as the launcher would report it.
	cmd := exec.Command("sh", "-c", "exit 3")
	_ = cmd.Run()
	if cmd.ProcessState.String() != "exit status 3" {
		t.Fatal(cmd.ProcessState.String())
	}
}
