package policy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/release"
)

// SBXShellDigest is the stock template pin (chat/internal/release). The sbx
// version is not pinned: ClusterFacts only requires that the executable
// answers `sbx version` like sbx does.
const SBXShellDigest = release.StockTemplateDigest

// TierSBX is the isolation tier the SBX inspector reports: a Docker
// Sandboxes microVM per sandbox.
const TierSBX = "sbx"

// CLIRunner executes the pinned SBX executable and returns stdout.
type CLIRunner func(args []string, denied bool) (string, error)

// ErrSettingUndefined is what the runner returns when sbx answers a
// `settings` command with `setting "…" is not defined`: this sbx build does
// not have the feature the setting governs, so there is nothing to disable
// and the host check treats it as satisfied.
var ErrSettingUndefined = errors.New("SBX setting not defined by this sbx")

// SbxInspector is the SandboxInspector of the sbx shapes. It checks actual
// daemon policy and identity through the pinned sbx executable; no guest
// report or worker flag contributes evidence. The runtime identity pin
// (runtime-identities.json) and the sandbox-scoped network rules are its
// vocabulary; the verifier sees runtime-neutral facts.
type SbxInspector struct {
	Executable    string
	ManageNetwork bool
	Runner        CLIRunner
	// ShellDigest is the pinned Warden guest image digest. A verified runtime
	// may run it or the stock mountless shell template it is built from, so
	// sandboxes created before the image was installed keep working.
	ShellDigest string
	// StockDigests are the digests the stock template may report on this
	// host architecture (release.StockTemplateDigests): the index digest, and
	// on arm64 also the arm64 image manifest inside it. Nil means the index
	// digest only.
	StockDigests []string
	// StorageFailed is told when the pin file cannot be written; the
	// registry then refuses every proof.
	StorageFailed func()
	pinsPath      string
	mu            sync.Mutex
	pins          map[string]string
	// granted remembers the endpoint each runtime was last granted, which
	// Facts probes; DenyEgress forgets it.
	granted map[string]GatewayEndpoint
}

// NewSbxInspector loads the runtime identity pins kept under state.
func NewSbxInspector(state, executable string, manageNetwork bool, runner CLIRunner) (*SbxInspector, error) {
	i := &SbxInspector{Executable: executable, ManageNetwork: manageNetwork, Runner: runner, ShellDigest: SBXShellDigest,
		StockDigests: release.StockTemplateDigests(runtime.GOARCH),
		pinsPath:     filepath.Join(state, "runtime-identities.json"), pins: map[string]string{}, granted: map[string]GatewayEndpoint{}}
	if i.Runner == nil {
		i.Runner = i.run
	}
	if raw, err := os.ReadFile(i.pinsPath); err == nil {
		parsed, err := StrictJSON(raw)
		if err != nil {
			return nil, err
		}
		pins, _ := parsed.(map[string]any)
		for name, value := range pins {
			id, _ := value.(string)
			i.pins[name] = id
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return i, nil
}

func (i *SbxInspector) run(args []string, denied bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, i.Executable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, limit: 1024*1024 + 1}
	cmd.Stderr = &limitedWriter{w: &stderr, limit: 4096}
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !(denied && errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
			if len(args) > 0 && args[0] == "settings" && strings.Contains(stderr.String(), "is not defined") {
				return "", ErrSettingUndefined
			}
			return "", errors.New("SBX inspection failed")
		}
	}
	if stdout.Len() > 1024*1024 {
		return "", errors.New("SBX inspection failed")
	}
	return stdout.String(), nil
}

func (i *SbxInspector) json(args []string, denied bool) (map[string]any, error) {
	out, err := i.Runner(args, denied)
	if err != nil {
		return nil, err
	}
	parsed, err := StrictJSON([]byte(out))
	if err != nil {
		return nil, err
	}
	value, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("unrecognized policy schema")
	}
	return value, nil
}

var linuxDefaultDeny = map[string]any{
	"id": "default-deny-all", "name": "default-deny-all", "policy_name": "default-deny-all", "scope": "global",
	"applies_to": "all", "resource_type": "network", "decision": "deny", "resources": []any{"**"}, "origin": "local",
	"layer": "local", "status": "active", "editable": false,
}

func (i *SbxInspector) rules(name string) ([]map[string]any, error) {
	args := []string{"policy", "ls"}
	if name != "" {
		args = append(args, name)
	}
	args = append(args, "--json", "--include-inactive")
	result, err := i.json(args, false)
	if err != nil {
		return nil, err
	}
	list, ok := result["rules"].([]any)
	if !sameKeys(result, "rules") || !ok {
		return nil, errors.New("unrecognized policy schema")
	}
	var relevant []map[string]any
	for _, item := range list {
		rule, _ := item.(map[string]any)
		kind, _ := rule["resource_type"].(string)
		if kind == "filesystem:read" || kind == "filesystem:write" {
			continue
		}
		if kind != "network" || rule["status"] != "active" || rule["layer"] != "local" {
			return nil, errors.New("unknown or governed network policy")
		}
		// Linux SBX 0.42.1 materializes its immutable implicit-deny sentinel
		// in policy ls. It grants nothing; check still proves implicit denial
		// and absence of governance independently.
		if jsonEqual(rule, linuxDefaultDeny) {
			continue
		}
		if name == "" && rule["scope"] != "global" {
			continue
		}
		relevant = append(relevant, rule)
	}
	return relevant, nil
}

func (i *SbxInspector) check(name, target string, allowed bool) error {
	args := []string{"policy", "check", "network", "--json"}
	if name != "" {
		args = append(args, "--sandbox", name)
	}
	args = append(args, target)
	result, err := i.json(args, !allowed)
	if err != nil {
		return err
	}
	governance, _ := result["governance"].(map[string]any)
	if result["allowed"] != allowed || !sameKeys(governance, "active") || governance["active"] != false {
		return errors.New("policy result or governance mismatch")
	}
	if !allowed && result["deny_kind"] != "implicit" {
		return errors.New("default-deny policy required")
	}
	return nil
}

// pin records the sandbox UUID the first time and refuses a different one
// afterwards: a replaced sandbox under a registered name is fatal.
func (i *SbxInspector) pin(name string, id any) (string, error) {
	uuid, ok := id.(string)
	if !ok || uuid == "" {
		return "", errors.New("missing runtime UUID")
	}
	if existing, ok := i.pins[name]; ok {
		if existing != uuid {
			return "", errors.New("runtime UUID changed")
		}
		return uuid, nil
	}
	updated := map[string]any{}
	for k, val := range i.pins {
		updated[k] = val
	}
	updated[name] = uuid
	if err := atomicWrite(i.pinsPath, i.pinsPath+".tmp", []byte(Dumps(updated))); err != nil {
		if i.StorageFailed != nil {
			i.StorageFailed()
		}
		return "", err
	}
	i.pins[name] = uuid
	return uuid, nil
}

// ClusterFacts verifies the host-wide invariants: pinned daemon version,
// safe settings, no MCP servers, no global network permissions, implicit
// denial.
func (i *SbxInspector) ClusterFacts(context.Context) error {
	version, err := i.Runner([]string{"version"}, false)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(version), "sbx version: ") {
		return errors.New("not an sbx executable: `sbx version` did not answer")
	}
	for _, setting := range []struct {
		key      string
		required any
	}{{"ssh.agentForwardingEnabled", false}, {"proxy.sandbox", "direct"}} {
		result, err := i.json([]string{"settings", "get", "--json", setting.key}, false)
		if errors.Is(err, ErrSettingUndefined) {
			continue
		}
		if err != nil {
			return err
		}
		if result["key"] != setting.key || result["value"] != setting.required {
			return errors.New("unsafe SBX setting")
		}
	}
	mcp, err := i.json([]string{"mcp", "ls", "--json"}, false)
	if err != nil {
		return err
	}
	servers, ok := mcp["servers"].([]any)
	gateway, _ := mcp["gateway"].(map[string]any)
	if !ok || len(servers) != 0 || gateway["local"] != true {
		return errors.New("host MCP gateway must have no servers")
	}
	global, err := i.rules("")
	if err != nil {
		return err
	}
	if len(global) > 0 {
		return errors.New("global network permissions unsupported")
	}
	return i.check("", "example.com:443", false)
}

// Facts inspects the sandbox: its inventory row, its profile (agent, image,
// kits, mounts, workspace), its identity pin, and its egress state probed
// against the endpoint GrantEgress established (or the bootstrap deny).
func (i *SbxInspector) Facts(_ context.Context, identity map[string]string) (RuntimeFacts, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	facts := RuntimeFacts{Tier: TierSBX}
	name := identity["runtimeName"]
	inventory, err := i.json([]string{"ls", "--json"}, false)
	if err != nil {
		return facts, err
	}
	rows, ok := inventory["sandboxes"].([]any)
	if !ok {
		return facts, errors.New("invalid runtime inventory")
	}
	var matching []map[string]any
	for _, item := range rows {
		row, _ := item.(map[string]any)
		if row["name"] == name {
			matching = append(matching, row)
		}
	}
	if len(matching) > 1 {
		return facts, errors.New("ambiguous runtime identity")
	}
	if len(matching) == 0 {
		facts.Egress = EgressDenied
		return facts, nil
	}
	facts.Present = true
	row := matching[0]
	if row["agent"] != "shell" || (row["status"] != "running" && row["status"] != "stopped") {
		return facts, errors.New("unsupported runtime")
	}
	details, err := i.json([]string{"inspect", name, "--json"}, false)
	if err != nil {
		return facts, err
	}
	kits, _ := details["kits"].([]any)
	daemonVersion, _ := details["daemon_version"].(string)
	facts.ImageDigest, _ = details["image_digest"].(string)
	if daemonVersion == "" || details["agent"] != "shell" || !i.allowedImage(details["image_digest"], identity["imageDigest"]) || kits == nil || len(kits) != 0 {
		return facts, errors.New("unsupported runtime profile")
	}
	for _, k := range []string{"workspace", "workspaces", "mounts", "static_mcp"} {
		if truthy(details[k]) {
			return facts, errors.New("unsupported runtime profile")
		}
	}
	if facts.Identity, err = i.pin(name, row["id"]); err != nil {
		return facts, err
	}
	facts.Egress = EgressDenied
	if endpoint, granted := i.granted[name]; granted {
		if err = i.check(name, "localhost:"+itoa(endpoint.Port), true); err != nil {
			return facts, err
		}
		facts.Egress = EgressGateway
	}
	for _, target := range []string{"example.com:443", "api.openai.com:443", "1.1.1.1:443", "localhost:18765"} {
		if err = i.check(name, target, false); err != nil {
			return facts, err
		}
	}
	return facts, nil
}

// GrantEgress moves a sandbox from its bootstrap deny rule to the single
// gateway-only allow rule, or confirms that rule is already the only one.
func (i *SbxInspector) GrantEgress(_ context.Context, identity map[string]string, gateway GatewayEndpoint) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	name := identity["runtimeName"]
	resource := "localhost:" + itoa(gateway.Port)
	rules, err := i.rules(name)
	if err != nil {
		return err
	}
	var bootstrap, allowed []map[string]any
	for _, rule := range rules {
		if rule["scope"] != "sandbox:"+name || rule["sandbox_id"] != name || rule["origin"] != "scoped" {
			return errors.New("policy outside owned sandbox")
		}
		switch {
		case rule["decision"] == "deny" && jsonEqual(rule["resources"], []any{"**"}):
			bootstrap = append(bootstrap, rule)
		case rule["decision"] == "allow" && jsonEqual(rule["resources"], []any{resource}):
			allowed = append(allowed, rule)
		default:
			return errors.New("unexpected sandbox permission")
		}
	}
	if i.ManageNetwork && len(bootstrap) > 0 {
		if len(allowed) == 0 {
			if _, err = i.Runner([]string{"policy", "allow", "network", "--sandbox", name, resource}, false); err != nil {
				return err
			}
		}
		// Deny remains active while the sole gateway exception is added.
		if _, err = i.Runner([]string{"policy", "rm", "network", "--sandbox", name, "--resource", "**"}, false); err != nil {
			return err
		}
		rules, err = i.rules(name)
		if err != nil {
			return err
		}
		if len(rules) != 1 || rules[0]["decision"] != "allow" || !jsonEqual(rules[0]["resources"], []any{resource}) {
			return errors.New("gateway policy transition failed")
		}
	} else if len(bootstrap) > 0 || len(allowed) != 1 {
		return errors.New("gateway-only policy not established")
	}
	i.granted[name] = gateway
	return nil
}

// DenyEgress re-applies the bootstrap deny rule to a sandbox this inspector
// pinned. An unpinned name was never proved to be Warden's, so its policy
// is not touched.
func (i *SbxInspector) DenyEgress(_ context.Context, identity map[string]string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	name := identity["runtimeName"]
	delete(i.granted, name)
	if _, pinned := i.pins[name]; !i.ManageNetwork || !pinned {
		return nil
	}
	// Worker must also stop on failed readiness; no success is reported.
	_, err := i.Runner([]string{"policy", "deny", "network", "--sandbox", name, "**"}, false)
	return err
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if n, ok := asNumber(v); ok {
		return n != 0
	}
	return true
}

// ValidImageDigest reports whether value is a sha256 OCI digest.
func ValidImageDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, c := range value[len("sha256:"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// allowedImage accepts the stock shell template, under any digest it may
// report on this architecture, and the pinned guest image.
// allowedImage is the image pin: the stock or pinned guest image, or the
// snapshot image the runner declared for this binding (a workspace copy
// or a regeneration it made from a verified guest; registry.go
// imageDigestShape).
func (i *SbxInspector) allowedImage(digest any, declared string) bool {
	value, ok := digest.(string)
	if !ok {
		return false
	}
	if value == SBXShellDigest || value == i.ShellDigest {
		return true
	}
	if declared != "" && value == declared {
		return true
	}
	for _, stock := range i.StockDigests {
		if value == stock {
			return true
		}
	}
	return false
}
