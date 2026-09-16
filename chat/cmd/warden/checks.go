package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// check is one doctor line: what was checked, whether it passed, what was
// seen and the exact command or action that fixes it.
type check struct {
	Name   string
	OK     bool
	Detail string
	Fix    string
}

func pass(name, detail string) check      { return check{Name: name, OK: true, Detail: detail} }
func fail(name, detail, fix string) check { return check{Name: name, Detail: detail, Fix: fix} }

// sbxVersionPrefix is what the policy verifier requires `sbx version` to
// print; no particular version is required.
var sbxVersionPrefix = "sbx version: "

// linuxDefaultDeny is the immutable implicit-deny sentinel Linux SBX 0.42.1
// materialises in `policy ls`; copied from chat/internal/policy/verifier.go.
var linuxDefaultDeny = map[string]any{
	"id": "default-deny-all", "name": "default-deny-all", "policy_name": "default-deny-all", "scope": "global",
	"applies_to": "all", "resource_type": "network", "decision": "deny", "resources": []any{"**"}, "origin": "local",
	"layer": "local", "status": "active", "editable": false,
}

// hostSettings are the two settings the verifier requires.
var hostSettings = []struct {
	key      string
	required any
	value    string // the literal for `settings set`
}{
	{"ssh.agentForwardingEnabled", false, "false"},
	{"proxy.sandbox", "direct", "direct"},
}

// hostChecks runs, in order and with the same commands and acceptance rules,
// every host invariant SbxCliVerifier.hostChecks enforces: an sbx
// executable, the two safe settings, no MCP servers, no global network rules and
// implicit denial. The verifier stops at the first failure; doctor runs them
// all so one report lists every remediation.
func hostChecks(s *sbxCLI) []check {
	var out []check
	version, err := s.run([]string{"version"}, false)
	switch {
	case err != nil:
		out = append(out, fail("sbx version", err.Error(), "install sbx (Docker Sandboxes) and make sure "+s.wrapper+" can run it"))
	case !strings.HasPrefix(strings.TrimSpace(version), sbxVersionPrefix):
		out = append(out, fail("sbx version", strings.TrimSpace(firstLine(version)), "the executable is not sbx: `sbx version` must print `"+sbxVersionPrefix+"…`"))
	default:
		out = append(out, pass("sbx version", strings.TrimSpace(firstLine(version))))
	}
	for _, setting := range hostSettings {
		name := "sbx setting " + setting.key
		fix := fmt.Sprintf("%s settings set %s %s (then %s daemon restart if it reports a restart)", s.wrapper, setting.key, setting.value, s.wrapper)
		result, err := s.jsonObject([]string{"settings", "get", "--json", setting.key}, false)
		switch {
		case settingUndefined(err):
			out = append(out, pass(name, "not defined by this sbx (the feature it governs is absent)"))
		case err != nil:
			out = append(out, fail(name, err.Error(), fix))
		case result["key"] != setting.key || result["value"] != setting.required:
			out = append(out, fail(name, fmt.Sprintf("value is %s, verifier requires %s", jsonText(result["value"]), jsonText(setting.required)), fix))
		default:
			out = append(out, pass(name, jsonText(result["value"])))
		}
	}
	mcp, err := s.jsonObject([]string{"mcp", "ls", "--json"}, false)
	if err != nil {
		fixText := "make sure the SBX daemon in Warden's namespace is running: " + s.wrapper + " daemon start --policy deny-all --detach"
		if notSignedIn(err) {
			fixText = "sign Warden's namespace in to Docker from your own terminal: " + s.wrapper + " login"
		}
		out = append(out, fail("sbx mcp inventory", err.Error(), fixText))
	} else {
		servers, ok := mcp["servers"].([]any)
		gateway, _ := mcp["gateway"].(map[string]any)
		switch {
		case !ok || gateway["local"] != true:
			out = append(out, fail("sbx mcp inventory", "the MCP gateway is not local ("+jsonText(mcp["gateway"])+")", "the host MCP gateway must be local with no servers; do not sign the Warden namespace into a hosted MCP control plane"))
		case len(servers) != 0:
			out = append(out, fail("sbx mcp inventory", fmt.Sprintf("%d MCP server(s) registered: %s", len(servers), serverNames(servers)), "remove each with "+s.wrapper+" mcp rm NAME; the verifier refuses any registered MCP server"))
		default:
			out = append(out, pass("sbx mcp inventory", "no servers, local gateway"))
		}
	}
	global, err := globalRules(s)
	switch {
	case err != nil:
		out = append(out, fail("sbx global network policy", err.Error(), "the policy listing must contain only local, active network rules (and the Linux default-deny sentinel); remove governed or unknown rules with "+s.wrapper+" policy rm network --id ID"))
	case len(global) > 0:
		out = append(out, fail("sbx global network policy", fmt.Sprintf("%d global network rule(s): %s", len(global), ruleIDs(global)), "remove every global network rule with "+s.wrapper+" policy rm network --id ID; Warden allows traffic per sandbox only"))
	default:
		out = append(out, pass("sbx global network policy", "no global rules"))
	}
	out = append(out, implicitDenial(s))
	return out
}

// globalRules mirrors SbxCliVerifier.rules("").
func globalRules(s *sbxCLI) ([]map[string]any, error) {
	result, err := s.jsonObject([]string{"policy", "ls", "--json", "--include-inactive"}, false)
	if err != nil {
		return nil, err
	}
	list, ok := result["rules"].([]any)
	if len(result) != 1 || !ok {
		return nil, fmt.Errorf("unrecognized policy schema (keys %s)", keysOf(result))
	}
	var relevant []map[string]any
	for _, item := range list {
		rule, _ := item.(map[string]any)
		kind, _ := rule["resource_type"].(string)
		if kind == "filesystem:read" || kind == "filesystem:write" {
			continue
		}
		if kind != "network" || rule["status"] != "active" || rule["layer"] != "local" {
			return nil, fmt.Errorf("unknown or governed network policy %s (resource_type %s, status %s, layer %s)", jsonText(rule["id"]), jsonText(rule["resource_type"]), jsonText(rule["status"]), jsonText(rule["layer"]))
		}
		if jsonEqual(rule, linuxDefaultDeny) {
			continue
		}
		if rule["scope"] != "global" {
			continue
		}
		relevant = append(relevant, rule)
	}
	return relevant, nil
}

// implicitDenial mirrors SbxCliVerifier.check("", "example.com:443", false).
func implicitDenial(s *sbxCLI) check {
	const name = "sbx implicit denial"
	fix := "the global policy must deny by default with no governance: stop the daemon in Warden's namespace and start it with " + s.wrapper + " daemon start --policy deny-all --detach"
	result, err := s.jsonObject([]string{"policy", "check", "network", "--json", "example.com:443"}, true)
	if err != nil {
		return fail(name, err.Error(), fix)
	}
	governance, _ := result["governance"].(map[string]any)
	if result["allowed"] != false || len(governance) != 1 || governance["active"] != false {
		return fail(name, "example.com:443 allowed="+jsonText(result["allowed"])+", governance="+jsonText(result["governance"]), fix)
	}
	if result["deny_kind"] != "implicit" {
		return fail(name, "denied by "+jsonText(result["deny_kind"])+", not implicitly", fix)
	}
	return pass(name, "example.com:443 denied implicitly, no governance")
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func keysOf(m map[string]any) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

func serverNames(servers []any) string {
	var names []string
	for _, s := range servers {
		row, _ := s.(map[string]any)
		if n, ok := row["name"].(string); ok {
			names = append(names, n)
		} else {
			names = append(names, jsonText(s))
		}
	}
	return strings.Join(names, ", ")
}

func ruleIDs(rules []map[string]any) string {
	var ids []string
	for _, r := range rules {
		ids = append(ids, fmt.Sprintf("%s %s %s", jsonText(r["id"]), jsonText(r["decision"]), jsonText(r["resources"])))
	}
	return strings.Join(ids, "; ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// notSignedIn recognises the daemon's answer when its Docker session is
// missing (HTTP 401 with "not authenticated").
// settingUndefined reports sbx's `setting "…" is not defined` answer: the
// running sbx has no such setting, so the feature it would disable is absent.
func settingUndefined(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not defined")
}

func notSignedIn(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "401") || strings.Contains(text, "not authenticated")
}
