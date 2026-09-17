package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type managedSandbox struct {
	SandboxInfo
	Repository         string
	RepositoryCheckout string
	RepositoryReady    bool
	PrincipalID        string
	Source             string
	Base               string
	Created            bool
	Creating           bool
	Installed          bool
	ClaudeInstalled    string // fingerprint of the host Claude executable copied into the guest
	ProxyCA            string // fingerprint of the gateway CA the guest trusts
	GuestCA            string // SHA-256 of a CA preinstalled by the guest image, as its manifest reports
	// pendingReport is a guest report captured while the guest was a spare;
	// fresh marks a guest this worker just created or adopted, which cannot
	// hold port publications yet. Neither is persisted.
	pendingReport  string
	fresh          bool
	LastActivity   time.Time
	Grant          GrantContext
	Active         *managedRun
	Reviewing      bool `json:"-"`
	residency      io.Closer
	previewAuditAt time.Time
}
type managedRun struct {
	ID        string
	ChatID    string
	Expires   time.Time
	Streaming bool
	cancel    context.CancelFunc
}
type chatBinding struct{ ID, ProjectID, SandboxID, RolloutPath, ThreadID string }
type managedState struct {
	Sandboxes    map[string]*managedSandbox
	Chats        map[string]*chatBinding
	Cancelled    map[string]bool
	Attachments  map[string]*PreviewAttachment
	Publications map[string]*publication
	Calls        map[string]savedAttachmentCall
	// Spares are booted guests not yet bound to any sandbox. They are
	// removed at startup (their keep-alive session died with the worker).
	Spares map[string]*spareSandbox
}

// spareSandbox is a pre-created guest waiting to be adopted by the next
// environment that needs a fresh sandbox without a repository clone.
type spareSandbox struct {
	Name      string
	Created   time.Time
	residency io.Closer
	report    string // guest report taken at boot, so adoption needs no round-trip
}

func newManagedState() *managedState {
	return &managedState{Sandboxes: map[string]*managedSandbox{}, Chats: map[string]*chatBinding{}, Cancelled: map[string]bool{}, Attachments: map[string]*PreviewAttachment{}, Publications: map[string]*publication{}, Calls: map[string]savedAttachmentCall{}, Spares: map[string]*spareSandbox{}}
}
func randomID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
func (w *Worker) defaultsLocked() {
	if w.managed == nil {
		w.managed = newManagedState()
	}
	if w.Runtime == nil {
		w.Runtime = &sbxRuntime{w}
	}
	if w.IdleTimeout <= 0 {
		w.IdleTimeout = 15 * time.Minute
	}
	if w.MaxResident <= 0 {
		w.MaxResident = 3
	}
	if w.Gate == nil {
		w.Gate = &UnixEnforcement{}
	}
	if w.Limits.Default.IsZero() {
		memory := w.MemoryMB
		if memory == 0 {
			memory = 1536
		}
		w.Limits.Default = Resources{CPUMilli: 1000, MemoryMB: memory}
	}
	if w.Limits.Max.IsZero() {
		w.Limits.Max = w.Limits.Default
	}
	if w.Limits.CPUStepMilli == 0 {
		w.Limits.CPUStepMilli = 1000
	}
	if _, isSBX := w.Runtime.(*sbxRuntime); isSBX {
		w.Limits.Restart = true
	}
}

// resourcesOf is the sandbox's size, the default for entries registered
// before sizes were recorded.
func (w *Worker) resourcesOf(s *managedSandbox) Resources {
	return s.Resources.Fill(w.Limits.Default)
}
func (w *Worker) saveManagedLocked() error {
	return atomicJSON(filepath.Join(w.Root, "managed-v2.json"), w.managed)
}
func (w *Worker) initializeManaged(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultsLocked()
	b, err := os.ReadFile(filepath.Join(w.Root, "managed-v2.json"))
	if err == nil {
		if err = json.Unmarshal(b, w.managed); err != nil {
			return fmt.Errorf("invalid worker registry: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, c := range w.managed.Chats {
		if s := w.managed.Sandboxes[c.SandboxID]; s != nil {
			w.registerControlBinding(Request{ProjectID: c.ProjectID, ChatID: c.ID, SandboxID: s.ID, PrincipalID: s.PrincipalID})
		}
	}
	// Only names registered in this worker root are ours. Startup never scans a prefix.
	for _, s := range w.managed.Sandboxes {
		if s.State != "stopped" && (s.Created || s.Creating) {
			if err = w.Runtime.Stop(ctx, s.RuntimeName); err != nil {
				return fmt.Errorf("could not stop interrupted registered sandbox: %w", err)
			}
		}
		if s.Active != nil && s.Grant.RunID != "" {
			endCtx, done := context.WithTimeout(ctx, 10*time.Second)
			_ = w.Gate.End(endCtx, s.Grant)
			done()
		}
		s.State = "stopped"
		s.Active = nil
	}
	for _, p := range w.managed.Publications {
		if p.State != "removed" {
			p.State = "stopped"
		}
		p.server = nil
		p.listener = nil
	}
	for _, a := range w.managed.Attachments {
		if a.State != "removed" {
			a.State = "stopped"
			a.URL = ""
		}
	}
	if w.managed.Spares == nil {
		w.managed.Spares = map[string]*spareSandbox{}
	}
	for name := range w.managed.Spares {
		// Its keep-alive died with the previous worker; a fresh spare is cheaper
		// than proving what state this one is in.
		_ = w.Runtime.Remove(ctx, name)
		delete(w.managed.Spares, name)
	}
	if err = w.saveManagedLocked(); err != nil {
		return err
	}
	go w.lifecycleLoop(ctx)
	return nil
}
func validIdentity(s string) bool { return identifier.MatchString(s) }
func (w *Worker) bindingLocked(r Request) (*managedSandbox, *chatBinding, error) {
	if !validIdentity(r.ProjectID) || !validIdentity(r.ChatID) || !validIdentity(r.SandboxID) {
		return nil, nil, errors.New("registered project, chat and sandbox IDs are required")
	}
	c := w.managed.Chats[r.ChatID]
	s := w.managed.Sandboxes[r.SandboxID]
	if c == nil || s == nil || c.ProjectID != r.ProjectID || c.SandboxID != r.SandboxID || s.ProjectID != r.ProjectID {
		return nil, nil, errors.New("chat is not authorized for this project sandbox")
	}
	if r.PrincipalID == "" || s.PrincipalID != r.PrincipalID {
		return nil, nil, errors.New("sandbox principal does not match")
	}
	return s, c, nil
}
func (w *Worker) bindLocked(r Request) (Response, error) {
	if !validIdentity(r.ProjectID) || !validIdentity(r.ChatID) || !validIdentity(r.SandboxID) || !validIdentity(r.PrincipalID) {
		return Response{}, errors.New("valid project, chat, sandbox and principal IDs required")
	}
	if _, err := publicRepositoryURL(r.Repository); err != nil {
		return Response{}, err
	}
	if r.Token != "" {
		return Response{}, errors.New("managed public checkout does not accept credentials")
	}
	if c := w.managed.Chats[r.ChatID]; c != nil {
		s, _, err := w.bindingLocked(r)
		if err != nil {
			return Response{}, err
		}
		if err = w.bindRepositoryLocked(s, r.Repository); err != nil {
			return Response{}, err
		}
		if err = w.saveManagedLocked(); err != nil {
			return Response{}, err
		}
		return w.statusLocked(r), nil
	}
	s := w.managed.Sandboxes[r.SandboxID]
	if s != nil && (s.ProjectID != r.ProjectID || s.PrincipalID != r.PrincipalID) {
		return Response{}, errors.New("sandbox belongs to another project or principal")
	}
	c := &chatBinding{ID: r.ChatID, ProjectID: r.ProjectID, SandboxID: r.SandboxID}
	if s == nil {
		if len(w.managed.Sandboxes) >= 32 {
			return Response{}, errors.New("worker has 32 retained sandboxes")
		}
		hash := sha256.Sum256([]byte(r.SandboxID))
		s = &managedSandbox{SandboxInfo: SandboxInfo{ID: r.SandboxID, ProjectID: r.ProjectID, RuntimeName: "wc-" + hex.EncodeToString(hash[:12]), Directory: "/home/agent/workspace", State: "stopped", Resources: w.Limits.Default}, PrincipalID: r.PrincipalID, LastActivity: w.now()}
		if r.Resources != nil {
			// The size a fresh workspace was created with; a size on a
			// later request is ignored, the sandbox has its own by then.
			resolved, err := w.Limits.Resolve(*r.Resources)
			if err != nil {
				return Response{}, fmt.Errorf("workspace size: %w", err)
			}
			s.Resources = resolved
		}
		if r.SessionID != "" {
			if !validIdentity(r.SessionID) {
				return Response{}, errors.New("invalid legacy session")
			}
			b, err := os.ReadFile(filepath.Join(w.Root, "sessions", r.SessionID+".json"))
			if err != nil {
				return Response{}, errors.New("legacy execution association is unavailable")
			}
			var old session
			if json.Unmarshal(b, &old) != nil || old.ProjectID != r.ProjectID {
				return Response{}, errors.New("legacy sandbox belongs to another project")
			}
			for _, other := range w.managed.Sandboxes {
				if other.RuntimeName == "ws-"+strings.ToLower(r.SessionID) {
					return Response{}, errors.New("legacy runtime is already registered")
				}
			}
			s.RuntimeName = "ws-" + strings.ToLower(r.SessionID)
			s.Directory = old.Directory
			s.Base = old.Base
			s.Created = true
			s.Installed = false
			s.ProxyCA = ""
			c.RolloutPath = old.RolloutPath
			c.ThreadID = r.ThreadID
		}
	}
	if err := w.bindRepositoryLocked(s, r.Repository); err != nil {
		return Response{}, err
	}
	w.managed.Sandboxes[s.ID] = s
	w.managed.Chats[c.ID] = c
	w.registerControlBinding(r)
	if err := w.saveManagedLocked(); err != nil {
		return Response{}, err
	}
	return w.statusLocked(r), nil
}
func (w *Worker) statusLocked(r Request) Response {
	s := w.managed.Sandboxes[r.SandboxID]
	info := s.SandboxInfo
	info.Resources = w.resourcesOf(s)
	limits := w.Limits
	result := Response{Version: ProtocolVersion, Sandbox: &info, Limits: &limits, Directory: s.Directory, Base: s.Base, Attachments: []PreviewAttachment{}}
	if c := w.managed.Chats[r.ChatID]; c != nil {
		result.RolloutPath = c.RolloutPath
	}
	for _, a := range w.managed.Attachments {
		if a.ChatID == r.ChatID && a.State != "removed" {
			result.Attachments = append(result.Attachments, *a)
		}
	}
	return result
}
func runKey(r Request) string { return r.ChatID + ":" + r.RunID }
func (w *Worker) runLocked(r Request) (*managedSandbox, *chatBinding, error) {
	s, c, err := w.bindingLocked(r)
	if err != nil {
		return nil, nil, err
	}
	if !validIdentity(r.RunID) {
		return nil, nil, errors.New("run ID required")
	}
	if w.wasExplicitlyCancelled(r) {
		return nil, nil, errors.New("run was cancelled")
	}
	if s.Active == nil || s.Active.ID != r.RunID || s.Active.ChatID != r.ChatID {
		return nil, nil, errors.New("request is not for the active registered run")
	}
	return s, c, nil
}
func (w *Worker) prepareLocked(ctx context.Context, r Request) (Response, error) {
	if err := ValidateAgent(r.Provider, r.Model); err != nil {
		return Response{}, err
	}
	if r.Provider == "claude" && w.ClaudePath == "" {
		return Response{}, errors.New("Claude runtime is not configured")
	}
	s, c, err := w.bindingLocked(r)
	if err != nil {
		return Response{}, err
	}
	if s.Reviewing {
		return Response{}, ErrBusy
	}
	if !validIdentity(r.RunID) {
		return Response{}, errors.New("run ID required")
	}
	if w.wasExplicitlyCancelled(r) {
		return Response{}, errors.New("run was cancelled")
	}
	active := 0
	for _, other := range w.managed.Sandboxes {
		if other.Active == nil {
			continue
		}
		if other.ID == s.ID {
			if other.Active.ID != r.RunID || other.Active.ChatID != r.ChatID {
				return Response{}, fmt.Errorf("%w: another run owns this sandbox", ErrBusy)
			}
		} else {
			active++
		}
	}
	if s.Active == nil && active >= w.parallelLimit() {
		return Response{}, fmt.Errorf("%w: agent session capacity reached", ErrBusy)
	}
	if s.Active != nil {
		return w.statusLocked(r), nil
	}
	if w.RuntimeDir == "" {
		return Response{}, errors.New("worker requires --runtime-dir with the pinned Linux Codex vendor bundle")
	}
	if r.OpenAIAPIKey != "" {
		return Response{}, errors.New("personal keys must be registered with Warden; sandbox secret injection is disabled")
	}
	if s.State != "running" {
		resident := 0
		for _, other := range w.managed.Sandboxes {
			if other.State == "running" {
				resident++
			}
		}
		if resident >= w.MaxResident {
			return Response{}, errors.New("resident sandbox capacity reached; stop an idle sandbox")
		}
		s.Generation = randomID()
	}
	ctx, releaseControl, err := w.registerRunControl(ctx, r)
	if err != nil {
		return Response{}, err
	}
	defer releaseControl()
	if !s.Created && !s.Creating && s.Source == "" && w.resourcesOf(s) == w.Limits.Default {
		// A booted spare becomes this sandbox's runtime before its identity is
		// registered, so the policy service only ever sees the final name.
		// Spares are booted at the default size; a workspace of another
		// size is created directly, since resizing a spare would cost a
		// regeneration on SBX.
		if spare := w.takeSpareLocked(); spare != nil {
			s.RuntimeName = spare.Name
			s.Created = true
			s.residency = spare.residency
			s.pendingReport = spare.report
			s.fresh = true
		}
	}
	grant := GrantContext{Provider: r.Provider, ProjectID: s.ProjectID, SandboxID: s.ID, RuntimeName: s.RuntimeName, Generation: s.Generation, ChatID: c.ID, RunID: r.RunID, PrincipalID: s.PrincipalID}
	if err = w.Gate.Register(ctx, grant); err != nil {
		w.failEnforcementLocked(s)
		return Response{}, err
	}
	phase := "runtime"
	if !s.Created {
		phase = "create"
	}
	if err = w.Gate.Check(ctx, grant, phase); err != nil {
		w.failEnforcementLocked(s)
		return Response{}, err
	}
	s.Grant = grant
	s.State = "starting"
	s.Active = &managedRun{ID: r.RunID, ChatID: c.ID, Expires: w.now().Add(60 * time.Second)}
	if err = w.saveManagedLocked(); err != nil {
		s.Active = nil
		return Response{}, err
	}
	fail := func(e error) (Response, error) {
		w.releaseResidencyLocked(s)
		s.Active = nil
		s.State = "error"
		if s.Created || s.Creating {
			stopCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
			if w.Runtime.Stop(stopCtx, s.RuntimeName) == nil {
				s.State = "stopped"
			}
			done()
		}
		w.invalidateLocked(s.ID)
		_ = w.saveManagedLocked()
		return Response{}, e
	}
	if !s.Created {
		s.Creating = true
		if err = w.saveManagedLocked(); err != nil {
			return fail(err)
		}
		if err = w.Runtime.Create(ctx, RuntimeSpec{Name: s.RuntimeName, Directory: s.Directory, Source: s.Source, Resources: w.resourcesOf(s)}); err != nil {
			return fail(err)
		}
		s.Created = true
		s.Creating = false
		s.fresh = true
		if err = w.saveManagedLocked(); err != nil {
			return fail(err)
		}
	}
	// Creation authorization is distinct from attested runtime networking.
	if err = w.Gate.Check(ctx, grant, "runtime"); err != nil {
		return fail(err)
	}
	report, err := w.probeGuestLocked(ctx, s)
	if err != nil {
		return fail(err)
	}
	guest := parseGuestReport(report)
	if !guest.ca {
		s.ProxyCA = ""
	}
	s.GuestCA = ""
	if guest.ca && guest.manifest != nil {
		s.GuestCA = guest.manifest.CA.Sha256
	}
	if !s.Installed && guest.codex && guest.manifest != nil {
		if host, herr := codexBundleMetadata(w.RuntimeDir); herr == nil && guest.manifest.Codex.Version == host.Version && guest.manifest.Codex.Target == host.Target {
			s.Installed = true
		}
	}
	if err = w.unpublishRemovedLocked(ctx, s); err != nil {
		return fail(err)
	}
	if r.Provider == "claude" {
		// The executable is large; copy it once per guest and again only when
		// the host file changes or the guest copy is missing.
		fingerprint, ferr := claudeFingerprint(w.ClaudePath)
		if ferr != nil {
			return fail(ferr)
		}
		if s.ClaudeInstalled != fingerprint && guest.claude && guest.manifest != nil && guest.manifest.Claude.Sha256 != "" {
			if digest, derr := w.hostClaudeDigest(fingerprint); derr == nil && digest == guest.manifest.Claude.Sha256 {
				s.ClaudeInstalled = fingerprint // the image ships this exact executable
			}
		}
		present := s.ClaudeInstalled == fingerprint && guest.claude
		if !present {
			s.ClaudeInstalled = ""
			if err = w.Runtime.Copy(ctx, s.RuntimeName, w.ClaudePath, "/tmp/warden-claude"); err != nil {
				return fail(err)
			}
			if _, err = w.Runtime.Exec(ctx, s.RuntimeName, "/tmp", "sudo", "chmod", "755", "/tmp/warden-claude"); err != nil {
				return fail(err)
			}
			s.ClaudeInstalled = fingerprint
		}
	}
	if !s.Installed {
		if _, err = w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "rm", "-rf", "--", "/tmp/warden-runtime-stage"); err != nil {
			return fail(err)
		}
		if err = w.Runtime.Copy(ctx, s.RuntimeName, w.RuntimeDir, "/tmp/warden-runtime-stage"); err != nil {
			return fail(fmt.Errorf("could not provision pinned Linux runtime bundle: %w", err))
		}
		if _, err = w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "sh", "-c", "chmod -R a+rX /tmp/warden-runtime-stage && test -x /tmp/warden-runtime-stage/bin/codex && test -x /tmp/warden-runtime-stage/bin/codex-code-mode-host && test -x /tmp/warden-runtime-stage/codex-path/rg && test -x /tmp/warden-runtime-stage/codex-resources/bwrap && rm -rf -- /tmp/warden-runtime && mv /tmp/warden-runtime-stage /tmp/warden-runtime"); err != nil {
			return fail(err)
		}
		s.Installed = true
	}

	if err = w.prepareRepositoryLocked(ctx, s); err != nil {
		return fail(err)
	}
	if err = w.Gate.Check(ctx, grant, "runtime"); err != nil {
		return fail(err)
	}
	s.Active.Expires = w.now().Add(60 * time.Second)

	if r.ThreadID != "" {
		if !validIdentity(r.ThreadID) {
			return fail(errors.New("invalid provider thread ID"))
		}
		if c.ThreadID != "" && c.ThreadID != r.ThreadID {
			return fail(errors.New("provider thread does not match registered chat history"))
		}
		// A guest-returned provider ID is not permission to copy a host rollout.
		// Explicit legacy binding above preserves the worker's registered path.
		c.ThreadID = r.ThreadID
	}

	s.State = "running"
	s.previewAuditAt = w.now()
	s.LastActivity = w.now()
	if err = w.saveManagedLocked(); err != nil {
		return fail(err)
	}
	return w.statusLocked(r), nil
}
func (w *Worker) cancelLocked(r Request) error {
	s, _, err := w.bindingLocked(r)
	if err != nil {
		return err
	}
	if !validIdentity(r.RunID) {
		return errors.New("run ID required")
	}
	w.managed.Cancelled[runKey(r)] = true
	if s.Active != nil && s.Active.ID == r.RunID && s.Active.ChatID == r.ChatID {
		if s.Active.cancel != nil {
			s.Active.cancel()
		} else {
			s.Active = nil
		}
	}
	return w.saveManagedLocked()
}
func (w *Worker) dispatch(ctx context.Context, r Request) (Response, error) {
	if r.Operation == "cancel" {
		return Response{}, w.cancelManaged(r)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultsLocked()
	if r.Operation == "bind-chat" {
		return w.bindLocked(r)
	}
	s, _, err := w.bindingLocked(r)
	if err != nil {
		return Response{}, err
	}
	switch r.Operation {
	case "status":
		return w.statusLocked(r), nil
	case "prepare":
		return w.prepareLocked(ctx, r)
	case "cancel":
		return Response{}, w.cancelLocked(r)
	case "activity":
		s.LastActivity = w.now()
		return w.statusLocked(r), w.saveManagedLocked()
	case "stop":
		if s.Active != nil {
			return Response{}, errors.New("sandbox has an active run")
		}
		err = w.stopLocked(ctx, s)
		return w.statusLocked(r), err
	case "remove":
		if s.Active != nil {
			return Response{}, errors.New("sandbox has an active run")
		}
		return w.removeSandboxLocked(ctx, s)
	case "resize":
		if s.Active != nil {
			return Response{}, errors.New("sandbox has an active run")
		}
		err = w.resizeLocked(ctx, s, r)
		return w.statusLocked(r), err
	case "file", "stat", "image-file", "proposal-file":
		if s.State != "running" {
			return Response{}, errors.New("sandbox is stopped; resume the chat before reading files")
		}
		if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
			return Response{}, err
		}
		script := fileScript
		if r.Operation == "image-file" {
			script = imageFileScript
		}
		if r.Operation == "proposal-file" {
			script = strings.ReplaceAll(imageFileScript, "8*1024*1024", "2*1024*1024")
		}
		raw, e := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "python3", "-c", script, s.Directory, r.Directory, r.Operation)
		if e != nil {
			return Response{}, e
		}
		var result Response
		if err = json.Unmarshal([]byte(raw), &result); err != nil {
			return Response{}, errors.New("invalid sandbox file response")
		}
		return result, nil
	case "host.import", "host.export":
		// Owner-approved copy of a host directory into the sandbox, or of
		// the sandbox's copy back over it (local installs only; the chat
		// enforces the mode, the runner enforces the path rules).
		if s.State != "running" {
			return Response{}, errors.New("sandbox is stopped; resume the chat first")
		}
		if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
			return Response{}, err
		}
		return w.hostDirectoryLocked(ctx, s, r)
	case "preview.attach":
		return w.attachLocked(ctx, r)
	case "preview.remove":
		return w.removeLocked(ctx, r)
	case "review.publish-plan":
		return w.publishPlanLocked(ctx, s, r)
	case "review.latest":
		return w.latestReviewLocked(s, r)
	case "review.read":
		review, err := w.readReviewLocked(s, r, r.CallID)
		return Response{Review: review}, err
	case "review.capture":
		return w.captureReviewLocked(ctx, s, r)
	default:
		return Response{}, errors.New("unsupported worker v2 operation")
	}
}
func (w *Worker) handle(parent context.Context, c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(c)
	line, err := readLine(reader, 1<<20)
	var r Request
	if err == nil {
		err = json.Unmarshal(line, &r)
	}
	send := func(res Response) {
		res.Version = ProtocolVersion
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = json.NewEncoder(c).Encode(res)
		_ = c.SetWriteDeadline(time.Time{})
	}
	if err != nil || r.Version != ProtocolVersion {
		send(Response{Error: "invalid or incompatible worker protocol; protocol 2 required"})
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	slots := w.ordinarySlots
	if r.Operation == "cancel" || r.Operation == "stats" || r.Operation == "health" || r.Operation == "status" || r.Operation == "activity" {
		slots = w.controlSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		send(Response{Error: "worker request capacity reached; retry", ErrorCode: "busy"})
		return
	}
	if r.Operation == "health" {
		send(Response{Output: "sbx protocol 2; execution requires verified Warden readiness", Revision: w.Revision})
		return
	}
	if r.Operation == "stats" {
		sample, err := w.metrics.Snapshot()
		if err != nil {
			send(Response{Error: "execution host metrics unavailable"})
			return
		}
		w.mu.Lock()
		active := 0
		if w.managed != nil {
			for _, s := range w.managed.Sandboxes {
				if s.Active != nil {
					active++
				}
			}
		}
		w.mu.Unlock()
		send(Response{Stats: &sample, ActiveSessions: active, SessionLimit: w.parallelLimit(), Revision: w.Revision})
		return
	}
	if r.Operation == "stream" {
		w.streamManaged(parent, c, reader, r, send)
		return
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	res, err := w.dispatch(ctx, r)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, ErrBusy) {
			res.ErrorCode = "busy"
		}
	}
	send(res)
}
func (w *Worker) streamManaged(parent context.Context, conn net.Conn, reader *bufio.Reader, r Request, send func(Response)) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.mu.Lock()
	w.defaultsLocked()
	s, _, err := w.runLocked(r)
	if err == nil && s.Active.Streaming {
		err = fmt.Errorf("%w: run already has an agent stream", ErrBusy)
	}
	admitted := err == nil
	if err == nil {
		var releaseControl func()
		ctx, releaseControl, err = w.registerRunControl(ctx, r)
		if err == nil {
			defer releaseControl()
		}
	}
	if err == nil && r.OpenAIAPIKey != "" {
		err = errors.New("personal keys must be registered with Warden")
	}
	if err == nil {
		err = w.Gate.Check(ctx, s.Grant, "runtime")
	}
	var broker BrokerConfig
	begun := false
	if err == nil {
		broker, err = w.Gate.Begin(ctx, s.Grant)
		begun = err == nil
	}
	var stream io.ReadWriteCloser
	if err == nil {
		broker.Provider = s.Grant.Provider
		broker.Model = r.Model
		broker.ThreadID = w.managed.Chats[r.ChatID].ThreadID
		if r.Provider != s.Grant.Provider || ValidateAgent(r.Provider, r.Model) != nil {
			err = errors.New("agent selection mismatch")
		} else if err = w.ensureProxyCALocked(ctx, s, broker.CACertificate); err == nil {
			stream, err = w.Runtime.Stream(ctx, s.RuntimeName, s.Directory, broker)
		}
	}
	if err != nil {
		if begun {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			_ = w.Gate.End(cleanup, s.Grant)
			done()
		}
		if admitted && s != nil && s.Active != nil && s.Active.ID == r.RunID {
			w.failEnforcementLocked(s)
			s.Active = nil
			_ = w.saveManagedLocked()
		}
		w.mu.Unlock()
		res := Response{Error: err.Error()}
		if errors.Is(err, ErrBusy) {
			res.ErrorCode = "busy"
		}
		send(res)
		return
	}
	grant := s.Grant
	var enforcementFailed atomic.Bool
	s.Active.Streaming = true
	s.Active.cancel = cancel
	s.Active.Expires = w.now().Add(60 * time.Second)
	res := w.statusLocked(r)
	res.APIKeyPlaceholder = broker.APIKeyPlaceholder
	_ = w.saveManagedLocked()
	w.mu.Unlock()
	stopConn := context.AfterFunc(ctx, func() { conn.Close(); stream.Close() })
	defer stopConn()
	defer func() { stream.Close(); w.finishManagedRun(r, grant, enforcementFailed.Load()) }()

	send(res)
	var copyOnce sync.Once
	go func() { _, _ = io.Copy(stream, reader); copyOnce.Do(cancel) }()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := w.Gate.Renew(ctx, grant); e != nil {
					log.Printf("sandbox %s run %s: permission renewal failed: %v", grant.SandboxID, grant.RunID, e)
					enforcementFailed.Store(true)
					cancel()
					return
				}
				w.mu.Lock()
				if s.Active != nil && s.Active.ID == r.RunID {
					s.Active.Expires = w.now().Add(60 * time.Second)
				}
				w.mu.Unlock()
			}
		}
	}()
	_, _ = io.Copy(conn, stream)
	copyOnce.Do(cancel)
}
func (w *Worker) invalidateLocked(id string) {
	if s := w.managed.Sandboxes[id]; s != nil {
		s.previewAuditAt = time.Time{}
	}
	for _, a := range w.managed.Attachments {
		if a.SandboxID == id && a.State != "removed" {
			a.State = "stopped"
			a.URL = ""
		}
	}
	for _, p := range w.managed.Publications {
		if p.SandboxID == id && p.State != "removed" {
			p.State = "stopped"
		}
	}
}
func (w *Worker) releaseResidencyLocked(s *managedSandbox) {
	if s.residency != nil {
		_ = s.residency.Close()
		s.residency = nil
	}
}
func (w *Worker) stopLocked(ctx context.Context, s *managedSandbox) error {
	w.releaseResidencyLocked(s)
	s.State = "stopping"
	w.invalidateLocked(s.ID)
	if err := w.saveManagedLocked(); err != nil {
		return err
	}
	if s.Created {
		if err := w.Runtime.Stop(ctx, s.RuntimeName); err != nil {
			s.State = "error"
			_ = w.saveManagedLocked()
			return err
		}
	}
	s.State = "stopped"
	return w.saveManagedLocked()
}

// resizeLocked records a new size and applies it to a created sandbox.
// A driver that replaces the instance leaves the sandbox stopped (its
// residency released, its previews published again by the next start);
// a live resize leaves it as it was. A sandbox not created yet only takes
// the size, its creation uses it.
func (w *Worker) resizeLocked(ctx context.Context, s *managedSandbox, r Request) error {
	if r.Resources == nil {
		return errors.New("resize requires a size")
	}
	resolved, err := w.Limits.Resolve(*r.Resources)
	if err != nil {
		return fmt.Errorf("workspace size: %w", err)
	}
	if resolved == w.resourcesOf(s) {
		s.Resources = resolved
		return w.saveManagedLocked()
	}
	if s.Creating {
		return errors.New("sandbox is being created")
	}
	if !s.Created {
		s.Resources = resolved
		return w.saveManagedLocked()
	}
	if w.Limits.Restart && s.State != "stopped" {
		if err := w.stopLocked(ctx, s); err != nil {
			return err
		}
	}
	restarted, err := w.Runtime.Resize(ctx, s.RuntimeName, resolved)
	if restarted {
		// The instance is gone whatever else happened; the next prepare
		// starts a new generation, which re-publishes its previews.
		w.releaseResidencyLocked(s)
		w.invalidateLocked(s.ID)
		s.State = "stopped"
	}
	if err != nil {
		if restarted {
			s.State = "error"
		}
		_ = w.saveManagedLocked()
		return err
	}
	s.Resources = resolved
	return w.saveManagedLocked()
}

// removeSandboxLocked deletes the environment: its runtime, workspace and every
// record bound to it. Published previews keep their 410 listeners so an old URL
// never reaches a different service while this worker is alive.
func (w *Worker) removeSandboxLocked(ctx context.Context, s *managedSandbox) (Response, error) {
	if err := w.stopLocked(ctx, s); err != nil {
		return Response{}, err
	}
	if s.Created || s.Creating {
		if err := w.Runtime.Remove(ctx, s.RuntimeName); err != nil {
			s.State = "error"
			_ = w.saveManagedLocked()
			return Response{}, fmt.Errorf("sandbox removal failed: %w", err)
		}
	}
	for id, a := range w.managed.Attachments {
		if a.SandboxID == s.ID {
			delete(w.managed.Attachments, id)
		}
	}
	for key, p := range w.managed.Publications {
		if p.SandboxID == s.ID {
			p.State = "removed"
			_ = key
		}
	}
	for id, c := range w.managed.Chats {
		if c.SandboxID == s.ID {
			delete(w.managed.Chats, id)
			control := w.control()
			control.mu.Lock()
			delete(control.bindings, id)
			control.mu.Unlock()
		}
	}
	delete(w.managed.Sandboxes, s.ID)
	info := s.SandboxInfo
	info.State = "removed"
	return Response{Version: ProtocolVersion, Sandbox: &info}, w.saveManagedLocked()
}
func (w *Worker) SweepIdle(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultsLocked()
	now := w.now()
	for _, s := range w.managed.Sandboxes {
		if s.Reviewing {
			continue
		}
		if s.Active != nil {
			if now.Before(s.Active.Expires) {
				continue
			}
			if s.Active.cancel != nil {
				s.Active.cancel()
				continue
			}
			s.Active = nil
		}
		// A published preview is ongoing work even after its agent turn ends.
		// Explicit stops still take precedence; removed or stale publications
		// do not keep an environment resident.
		if w.hasLivePreviewLocked(s) {
			continue
		}
		if s.State == "running" && !now.Before(s.LastActivity.Add(w.IdleTimeout)) {
			if err := w.stopLocked(ctx, s); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *Worker) hasLivePreviewLocked(s *managedSandbox) bool {
	for _, p := range w.managed.Publications {
		if p.SandboxID == s.ID && p.State == "available" && p.Generation == s.Generation {
			for _, a := range w.managed.Attachments {
				if a.SandboxID == s.ID && a.Port == p.Port && a.State == "available" {
					return true
				}
			}
		}
	}
	return false
}
func (w *Worker) lifecycleLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, s := range w.managed.Sandboxes {
			w.releaseResidencyLocked(s)
		}
		for _, p := range w.managed.Publications {
			if p.server != nil {
				p.server.Close()
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.auditPreviews(ctx)
			w.maintainSpares(ctx)
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = w.SweepIdle(callCtx)
			cancel()
		}
	}
}

func (w *Worker) failEnforcementLocked(s *managedSandbox) {
	w.releaseResidencyLocked(s)
	if s.Active != nil && s.Active.cancel != nil {
		s.Active.cancel()
	}
	w.invalidateLocked(s.ID)
	if s.Created || s.Creating {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.State = "error"
		if w.Runtime.Stop(ctx, s.RuntimeName) == nil {
			s.State = "stopped"
		}
	}
	_ = w.saveManagedLocked()
}

// The provider lease ends before runtime shutdown and admission release. Normal
// completion preserves preview processes; explicit cancellation stops the whole
// guest because killing the host SBX CLI cannot prove guest-root descendants died.
func (w *Worker) finishManagedRun(r Request, grant GrantContext, enforcementFailed bool) {
	endCtx, end := context.WithTimeout(context.Background(), 10*time.Second)
	endErr := w.Gate.End(endCtx, grant)
	end()
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.managed.Sandboxes[r.SandboxID]
	if s == nil || s.Generation != grant.Generation || s.Active == nil || s.Active.ID != r.RunID || s.Active.ChatID != r.ChatID {
		return
	}
	explicitCancel := w.wasExplicitlyCancelled(r)
	s.Active = nil
	s.LastActivity = w.now()
	if explicitCancel || endErr != nil || enforcementFailed {
		// A failed/expired broker request cannot consume the VM-stop deadline.
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = w.stopLocked(stopCtx, s)
		return
	}
	_ = w.saveManagedLocked()
}

// Expensive isolation attestation never holds the worker lock or a page request.
func (w *Worker) auditPreviews(ctx context.Context) {
	w.mu.Lock()
	var grants []GrantContext
	for _, s := range w.managed.Sandboxes {
		if s.State == "running" && w.hasLivePreviewLocked(s) && !w.now().Before(s.previewAuditAt.Add(time.Minute)) {
			grants = append(grants, s.Grant)
		}
	}
	w.mu.Unlock()
	for _, grant := range grants {
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := w.Gate.Check(checkCtx, grant, "runtime")
		cancel()
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		s := w.managed.Sandboxes[grant.SandboxID]
		if s != nil && s.State == "running" && s.Grant == grant {
			if err != nil {
				w.failEnforcementLocked(s)
			} else {
				s.previewAuditAt = w.now()
			}
		}
		w.mu.Unlock()
	}
}

// claudeFingerprint identifies the host Claude executable by size and
// modification time so a replaced binary is copied into guests again.
func claudeFingerprint(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Claude runtime is not readable: %w", err)
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()), nil
}

func (w *Worker) execOK(ctx context.Context, name, dir string, args ...string) (string, bool) {
	out, err := w.Runtime.Exec(ctx, name, dir, args...)
	return out, err == nil
}

// ensureProxyCALocked installs the broker's gateway CA into the guest once,
// and again only when the CA changes (rotation) or the guest lost its copy.
// A guest image that ships this exact certificate needs no install at all.
func (w *Worker) ensureProxyCALocked(ctx context.Context, s *managedSandbox, certificate string) error {
	if certificate == "" {
		return nil
	}
	fingerprint := hex.EncodeToString(func() []byte { sum := sha256.Sum256([]byte(certificate)); return sum[:] }())
	if s.ProxyCA == fingerprint {
		return nil
	}
	if s.GuestCA == fingerprint {
		s.ProxyCA = fingerprint
		return nil
	}
	s.ProxyCA = ""
	if err := w.Runtime.InstallCA(ctx, s.RuntimeName, certificate); err != nil {
		return err
	}
	s.ProxyCA = fingerprint
	return nil
}

// guestManifestPath is written by the Warden guest image build
// (deploy/guest) and lists what the image preinstalled.
const guestManifestPath = "/opt/warden/guest-manifest.json"

// guestReportScript runs as one guest round-trip during prepare.
const guestReportScript = `mkdir -p "$0" || exit 1
echo WARDEN-GUEST-BEGIN; cat ` + guestManifestPath + ` 2>/dev/null; echo; echo WARDEN-GUEST-END
test -f ` + guestCAPath + ` && echo ca-present || echo ca-absent
test -x /tmp/warden-runtime/bin/codex && echo codex-present || echo codex-absent
test -x /tmp/warden-claude && echo claude-present || echo claude-absent`

type guestManifest struct {
	Codex struct {
		Version string `json:"version"`
		Target  string `json:"target"`
	} `json:"codex"`
	Claude struct {
		Version string `json:"version"`
		Sha256  string `json:"sha256"`
	} `json:"claude"`
	CA struct {
		Sha256 string `json:"sha256"`
	} `json:"ca"`
}
type guestReport struct {
	manifest          *guestManifest
	ca, codex, claude bool
}

// parseGuestReport reads the prepare round-trip output. Anything the guest
// wrote is untrusted: an unparsable manifest is ignored and only exact
// digest matches against host files are acted on.
func parseGuestReport(report string) guestReport {
	var g guestReport
	if start := strings.Index(report, "WARDEN-GUEST-BEGIN\n"); start >= 0 {
		rest := report[start+len("WARDEN-GUEST-BEGIN\n"):]
		if end := strings.Index(rest, "WARDEN-GUEST-END"); end >= 0 {
			body := strings.TrimSpace(rest[:end])
			var m guestManifest
			if body != "" && len(body) <= 4096 && json.Unmarshal([]byte(body), &m) == nil {
				g.manifest = &m
			}
		}
	}
	g.ca = strings.Contains(report, "ca-present")
	g.codex = strings.Contains(report, "codex-present")
	g.claude = strings.Contains(report, "claude-present")
	return g
}

// hostClaudeDigest hashes the host Claude executable once per fingerprint.
func (w *Worker) hostClaudeDigest(fingerprint string) (string, error) {
	if w.claudeDigestKey == fingerprint && w.claudeDigest != "" {
		return w.claudeDigest, nil
	}
	file, err := os.Open(w.ClaudePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	w.claudeDigestKey, w.claudeDigest = fingerprint, hex.EncodeToString(hash.Sum(nil))
	return w.claudeDigest, nil
}

// takeSpareLocked hands out any ready spare, or nil.
func (w *Worker) takeSpareLocked() *spareSandbox {
	for name, spare := range w.managed.Spares {
		delete(w.managed.Spares, name)
		return spare
	}
	return nil
}

// maintainSpares starts creating one spare guest when fewer than Spares are
// ready. Creation runs outside the worker lock; a failure backs off for 30 s.
// Spares sit beside MaxResident on purpose: a booted idle guest uses a few
// hundred MiB, far below its limit, and it saves a 7 s boot per new chat.
func (w *Worker) maintainSpares(ctx context.Context) {
	w.mu.Lock()
	if w.Spares <= 0 || w.spareBusy || len(w.managed.Spares) >= w.Spares || w.now().Before(w.spareRetryAt) || ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	w.spareBusy = true
	w.mu.Unlock()
	name := "wc-spare-" + randomID()[:16]
	go func() {
		createCtx, done := context.WithTimeout(ctx, 2*time.Minute)
		defer done()
		var residency io.Closer
		err := w.Runtime.Create(createCtx, RuntimeSpec{Name: name, Directory: "/home/agent/workspace"})
		if err == nil {
			residency, err = w.Runtime.KeepAlive(name)
		}
		report := ""
		if err == nil {
			// Taken now so adoption needs no guest round-trip; a failure here
			// only means prepare takes the report itself.
			report, _ = w.Runtime.Exec(createCtx, name, "/tmp", "sh", "-c", guestReportScript, "/home/agent/workspace")
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.spareBusy = false
		if err != nil {
			log.Printf("spare sandbox %s: %v", name, err)
			w.spareRetryAt = w.now().Add(30 * time.Second)
			stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			_ = w.Runtime.Remove(stopCtx, name)
			stop()
			return
		}
		w.managed.Spares[name] = &spareSandbox{Name: name, Created: w.now(), residency: residency, report: report}
		_ = w.saveManagedLocked()
	}()
}

// probeGuestLocked performs the guest round-trips a run needs before its
// stream, concurrently: the guest report (workspace, manifest, presence of
// the CA and runtimes), the port-publication check, and the keep-alive
// session that holds the guest resident. Each is skipped when it is already
// known: a spare's report was taken at boot, a guest this worker just
// created or adopted cannot hold publications, and an adopted spare already
// has its keep-alive.
func (w *Worker) probeGuestLocked(ctx context.Context, s *managedSandbox) (string, error) {
	report, haveReport := s.pendingReport, s.pendingReport != ""
	fresh := s.fresh
	s.pendingReport, s.fresh = "", false
	var wg sync.WaitGroup
	var reportErr, mappingErr, keepErr error
	var residency io.Closer
	if !haveReport {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, reportErr = w.Runtime.Exec(ctx, s.RuntimeName, "/tmp", "sh", "-c", guestReportScript, s.Directory)
		}()
	}
	if !fresh {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mappingErr = w.verifyMappingsLocked(ctx, s, nil, false)
		}()
	}
	if s.residency == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			residency, keepErr = w.Runtime.KeepAlive(s.RuntimeName)
		}()
	}
	wg.Wait()
	if residency != nil {
		s.residency = residency // owned by the sandbox now, so a later failure releases it
	}
	for _, err := range []error{reportErr, mappingErr, keepErr} {
		if err != nil {
			return "", err
		}
	}
	return report, nil
}

// hostDirectoryLimit bounds what request_host_directory may copy.
const hostDirectoryLimit = 1 << 30

// hostDirectoryLocked validates a host directory and copies it into
// /home/agent/host/<name> (host.import) or the sandbox's copy back over the
// host directory, merging file by file (host.export). The path must be an
// absolute existing directory outside Warden's own state and under 1 GiB.
func (w *Worker) hostDirectoryLocked(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	host := filepath.Clean(r.Path)
	if !filepath.IsAbs(host) || host == "/" {
		return Response{}, errors.New("an absolute directory path is required")
	}
	info, err := os.Lstat(host)
	if err != nil || !info.IsDir() {
		return Response{}, errors.New("not an existing directory on this machine")
	}
	state := filepath.Dir(w.Root)
	if host == state || strings.HasPrefix(host, state+string(filepath.Separator)) {
		return Response{}, errors.New("Warden's own state directory cannot be shared")
	}
	var total int64
	err = filepath.WalkDir(host, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		if total > hostDirectoryLimit {
			return errors.New("directory exceeds 1 GiB")
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	name := filepath.Base(host)
	guest := "/home/agent/host/" + name
	if r.Operation == "host.export" {
		if _, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "test", "-d", guest); err != nil {
			return Response{}, errors.New("the sandbox has no copy of that directory; share it first")
		}
		if err := w.Runtime.CopyOut(ctx, s.RuntimeName, guest, filepath.Dir(host)); err != nil {
			return Response{}, err
		}
		return Response{Directory: guest, Output: host}, nil
	}
	if _, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "mkdir", "-p", "/home/agent/host"); err != nil {
		return Response{}, err
	}
	if err := w.Runtime.Copy(ctx, s.RuntimeName, host, "/home/agent/host"); err != nil {
		return Response{}, err
	}
	if _, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "chown", "-R", "agent:agent", "/home/agent/host"); err != nil {
		return Response{}, err
	}
	return Response{Directory: guest, Output: host}, nil
}
