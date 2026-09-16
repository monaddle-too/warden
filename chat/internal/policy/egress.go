package policy

import (
	"errors"
	"regexp"
)

// hostPattern accepts canonical public DNS names only (no IP literals,
// trailing dots, underscores or single labels), as the Python HOST regex did.
var hostPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

var egressCategories = map[string]bool{"ai": true, "dependencies": true, "development": true, "updates": true}
var egressMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}

// ValidHost mirrors HOST.fullmatch including the 253 character bound.
func ValidHost(host string) bool {
	return len(host) >= 1 && len(host) <= 253 && hostPattern.MatchString(host)
}

// ValidateEgress checks an egress policy document.
func ValidateEgress(value any) error {
	m, ok := asMap(value)
	if !ok || !sameKeys(m, "mode", "destinations") {
		return errors.New("invalid egress policy")
	}
	mode, _ := asString(m["mode"])
	if mode != "restricted" && mode != "public" {
		return errors.New("invalid egress policy")
	}
	rules, ok := asList(m["destinations"])
	if !ok || len(rules) > 256 {
		return errors.New("invalid destinations")
	}
	seen := map[string]bool{}
	for _, item := range rules {
		rule, ok := asMap(item)
		if !ok || !sameKeys(rule, "host", "methods", "category") {
			return errors.New("invalid destination rule")
		}
		host, ok := asString(rule["host"])
		if !ok || !ValidHost(host) || seen[host] {
			return errors.New("destinations must be unique exact DNS names")
		}
		seen[host] = true
		category, _ := asString(rule["category"])
		if !egressCategories[category] {
			return errors.New("invalid destination category")
		}
		methods, ok := asList(rule["methods"])
		if !ok || len(methods) == 0 {
			return errors.New("invalid destination methods")
		}
		for _, method := range methods {
			name, ok := asString(method)
			if !ok || !egressMethods[name] {
				return errors.New("invalid destination methods")
			}
		}
	}
	return nil
}

// EgressPermits applies an exact destination policy to a request.
func EgressPermits(policy map[string]any, host, method, scheme string, tls bool) bool {
	if !ValidHost(host) {
		return false
	}
	if mode, _ := asString(policy["mode"]); mode == "public" {
		return true
	}
	rules, _ := asList(policy["destinations"])
	for _, item := range rules {
		rule, _ := asMap(item)
		if h, _ := asString(rule["host"]); h != host {
			continue
		}
		if tls {
			category, _ := asString(rule["category"])
			return category == "updates"
		}
		if scheme != "https" {
			return false
		}
		methods, _ := asList(rule["methods"])
		for _, m := range methods {
			if name, _ := asString(m); name == method {
				return true
			}
		}
		return false
	}
	return false
}
