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

	"warden/chat/internal/release"
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
	pendingReport string
	fresh         bool
	// paths is the guest layout the last guest report described (the
	// manifest's paths object, or the SBX template's defaults); it is
	// taken again at every prepare, so it is not persisted.
	paths          GuestPaths
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
		w.Gate = &PolicyEnforcement{}
	}
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
		if p.Address == "" && p.HostPort != 0 {
			// Written before publications recorded their address: the
			// driver in use then published on the address it publishes on now.
			if s := w.managed.Sandboxes[p.SandboxID]; s != nil {
				if p.Address, err = w.Runtime.Address(ctx, s.RuntimeName); err != nil {
					return err
				}
			}
		}
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
	if reconciler, ok := w.Runtime.(Reconciler); ok {
		// Every registered guest is stopped and every registered spare gone;
		// a driver whose guests outlive the worker retires what else it finds
		// and keeps the registered workspaces.
		registered := make([]string, 0, len(w.managed.Sandboxes))
		for _, s := range w.managed.Sandboxes {
			registered = append(registered, s.RuntimeName)
		}
		if err = reconciler.Reconcile(ctx, registered); err != nil {
			return fmt.Errorf("runtime reconciliation: %w", err)
		}
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
		s = &managedSandbox{SandboxInfo: SandboxInfo{ID: r.SandboxID, ProjectID: r.ProjectID, RuntimeName: "wc-" + hex.EncodeToString(hash[:12]), Directory: "/home/agent/workspace", State: "stopped"}, PrincipalID: r.PrincipalID, LastActivity: w.now()}
		if r.SessionID != "" {
			// The protocol 1 worker whose ws-* sandboxes this adopted is gone.
			return Response{}, errors.New("legacy sandbox adoption is no longer supported")
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
	result := Response{Version: ProtocolVersion, Sandbox: &info, Directory: s.Directory, Base: s.Base, Attachments: []PreviewAttachment{}}
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
	if !s.Created && !s.Creating && s.Source == "" {
		// A booted spare becomes this sandbox's runtime before its identity is
		// registered, so the policy service only ever sees the final name.
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
		if err = w.Runtime.Create(ctx, RuntimeSpec{Name: s.RuntimeName, Directory: s.Directory, Source: s.Source, SandboxID: s.ID, Generation: s.Generation}); err != nil {
			return fail(err)
		}
		s.Created = true
		s.Creating = false
		s.fresh = true
		if err = w.saveManagedLocked(); err != nil {
			return fail(err)
		}
	}
	// The runtime must exist before its networking can be attested: a
	// pod-based driver recreates a stopped sandbox's pod here (under the
	// namespace's default deny), the SBX driver opens its residency session.
	if err = w.ensureResidencyLocked(ctx, s); err != nil {
		return fail(err)
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
	s.paths = guest.paths()
	if !guest.ca {
		s.ProxyCA = ""
	}
	s.GuestCA = ""
	if guest.ca && guest.manifest != nil {
		s.GuestCA = guest.manifest.CA.Sha256
	}
	if !s.Installed && guest.codex && guest.manifest != nil {
		// The image ships the pinned Codex release (its digest is verified by
		// the policy service), or it ships the same bundle the host holds.
		if guest.manifest.Codex.Version == release.CodexVersion && pinnedCodexTarget(guest.manifest.Codex.Target) {
			s.Installed = true
		} else if host, herr := codexBundleMetadata(w.RuntimeDir); w.RuntimeDir != "" && herr == nil && guest.manifest.Codex.Version == host.Version && guest.manifest.Codex.Target == host.Target {
			s.Installed = true
		}
	}
	if !s.Installed && w.RuntimeDir == "" {
		return fail(errors.New("the guest image does not ship the pinned Codex runtime and no host bundle is configured (runtimes.codex)"))
	}
	if err = w.unpublishRemovedLocked(ctx, s); err != nil {
		return fail(err)
	}
	if r.Provider == "claude" && w.ClaudePath == "" {
		// No host copy: the image must ship the pinned executable.
		if !(guest.claude && guest.manifest != nil && guest.manifest.Claude.Version == release.ClaudeVersion && pinnedClaudeDigest(guest.manifest.Claude.Sha256)) {
			return fail(errors.New("the guest image does not ship the pinned Claude executable and no host copy is configured (runtimes.claude)"))
		}
		s.ClaudeInstalled = "image:" + guest.manifest.Claude.Sha256
	} else if r.Provider == "claude" {
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
			if err = w.Runtime.Copy(ctx, s.RuntimeName, w.ClaudePath, s.paths.Claude); err != nil {
				return fail(err)
			}
			if _, err = w.Runtime.Exec(ctx, s.RuntimeName, "/tmp", "sudo", "chmod", "755", s.paths.Claude); err != nil {
				return fail(err)
			}
			s.ClaudeInstalled = fingerprint
		}
	}
	if !s.Installed {
		// The bundle is staged beside its final path and moved into place
		// once its executables are verified; both paths are the manifest's
		// (or the template's), validated by guestPath and passed as data.
		stage := s.paths.Codex + "-stage"
		if _, err = w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "rm", "-rf", "--", stage); err != nil {
			return fail(err)
		}
		if err = w.Runtime.Copy(ctx, s.RuntimeName, w.RuntimeDir, stage); err != nil {
			return fail(fmt.Errorf("could not provision pinned Linux runtime bundle: %w", err))
		}
		if _, err = w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "sudo", "sh", "-c", `chmod -R a+rX "$0" && test -x "$0/bin/codex" && test -x "$0/bin/codex-code-mode-host" && test -x "$0/codex-path/rg" && test -x "$0/codex-resources/bwrap" && rm -rf -- "$1" && mv "$0" "$1"`, stage, s.paths.Codex); err != nil {
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
		} else {
			stream, err = w.launchLocked(ctx, s, broker)
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

// launchLocked starts the agent stream with the guest layout the last
// report described and the trust decision for the broker's gateway CA: the
// guest already trusts it when its image shipped this exact certificate or
// an earlier run installed it and the guest still has the file, so the
// driver installs it once per guest, and again only after a rotation or a
// loss. The fingerprint is recorded once the launch succeeded, since the
// driver delivers trust as part of it.
func (w *Worker) launchLocked(ctx context.Context, s *managedSandbox, broker BrokerConfig) (io.ReadWriteCloser, error) {
	fingerprint := ""
	trusted := true
	if broker.CACertificate != "" {
		fingerprint = hex.EncodeToString(func() []byte { sum := sha256.Sum256([]byte(broker.CACertificate)); return sum[:] }())
		trusted = s.ProxyCA == fingerprint || s.GuestCA == fingerprint
		if !trusted {
			s.ProxyCA = ""
		}
	}
	stream, err := w.Runtime.Stream(ctx, s.RuntimeName, RunSpec{Directory: s.Directory, Broker: broker, Paths: s.paths.orDefaults(), TrustsCA: trusted})
	if err != nil {
		return nil, err
	}
	if fingerprint != "" {
		s.ProxyCA = fingerprint
	}
	return stream, nil
}

// guestManifestPath is written by the Warden guest image build
// (deploy/guest) and lists what the image preinstalled.
const guestManifestPath = "/opt/warden/guest-manifest.json"

// guestReportScript runs as one guest round-trip during prepare. The
// runtime presence checks look where the manifest's paths object says the
// runtimes are (the one-line manifest written by deploy/guest/write-manifest.sh)
// and fall back to the SBX template's /tmp layout, the same resolution
// guestReport.paths applies on the host side.
const guestReportScript = `mkdir -p "$0" || exit 1
echo WARDEN-GUEST-BEGIN; cat ` + guestManifestPath + ` 2>/dev/null; echo; echo WARDEN-GUEST-END
codex=$(sed -n 's/.*"paths": *{[^}]*"codex": *"\([^"]*\)".*/\1/p' ` + guestManifestPath + ` 2>/dev/null); [ -n "$codex" ] || codex=` + defaultCodexPath + `
claude=$(sed -n 's/.*"paths": *{[^}]*"claude": *"\([^"]*\)".*/\1/p' ` + guestManifestPath + ` 2>/dev/null); [ -n "$claude" ] || claude=` + defaultClaudePath + `
test -f ` + guestCAPath + ` && echo ca-present || echo ca-absent
test -x "$codex/bin/codex" && echo codex-present || echo codex-absent
test -x "$claude" && echo claude-present || echo claude-absent`

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
	Paths GuestPaths `json:"paths"`
	User  struct {
		Name string `json:"name"`
	} `json:"user"`
}
type guestReport struct {
	manifest          *guestManifest
	ca, codex, claude bool
}

// paths is the guest layout: the manifest's paths object with the SBX
// template's defaults filling whatever it leaves out or names unusably.
func (g guestReport) paths() GuestPaths {
	var p GuestPaths
	if g.manifest != nil {
		p = g.manifest.Paths
		p.User = g.manifest.User.Name
	}
	return p.orDefaults()
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
		spec := RuntimeSpec{Name: name, Directory: "/home/agent/workspace", Spare: true}
		err := w.Runtime.Create(createCtx, spec)
		if err == nil {
			residency, err = w.Runtime.Prepare(createCtx, spec)
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
// stream: first the driver's preparation when the guest is not resident
// (for SBX the keep-alive session that boots and holds the VM; for a pod
// driver the pod of this generation), then, concurrently, the guest report
// (workspace, manifest, presence of the CA and runtimes) and the
// port-publication check. Each is skipped when it is already known: a
// spare's report was taken at boot, a guest this worker just created or
// adopted cannot hold publications, and an adopted spare is already
// resident.
// ensureResidencyLocked prepares the runtime (the SBX keep-alive session, or
// the pod of the current generation) once per residency.
func (w *Worker) ensureResidencyLocked(ctx context.Context, s *managedSandbox) error {
	if s.residency != nil {
		return nil
	}
	residency, err := w.Runtime.Prepare(ctx, RuntimeSpec{Name: s.RuntimeName, Directory: s.Directory, Source: s.Source, SandboxID: s.ID, Generation: s.Generation})
	if residency != nil {
		s.residency = residency // owned by the sandbox now, so a later failure releases it
	}
	return err
}

func (w *Worker) probeGuestLocked(ctx context.Context, s *managedSandbox) (string, error) {
	report, haveReport := s.pendingReport, s.pendingReport != ""
	fresh := s.fresh
	s.pendingReport, s.fresh = "", false
	if err := w.ensureResidencyLocked(ctx, s); err != nil {
		return "", err
	}
	var wg sync.WaitGroup
	var reportErr, mappingErr error
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
	wg.Wait()
	for _, err := range []error{reportErr, mappingErr} {
		if err != nil {
			return "", err
		}
	}
	return report, nil
}

// pinnedCodexTarget reports whether a guest manifest's Codex target is one
// of the release's published bundles.
func pinnedCodexTarget(target string) bool {
	for _, bundle := range release.CodexBundles {
		if target != "" && strings.Contains(bundle.URL, "codex-package-"+target+".tar.gz") {
			return true
		}
	}
	return false
}

// pinnedClaudeDigest reports whether a guest manifest's Claude executable is
// one of the release's published builds.
func pinnedClaudeDigest(sha256 string) bool {
	for _, exe := range release.ClaudeExecutables {
		if sha256 != "" && exe.SHA256 == sha256 {
			return true
		}
	}
	return false
}
