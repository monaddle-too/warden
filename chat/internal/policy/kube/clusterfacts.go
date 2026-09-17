package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	api "warden/chat/internal/kube"
)

// ClusterFacts proves the cluster-wide invariants (decisions 11 and 12):
// the sandbox namespace with its Pod Security Admission and role labels,
// the three static NetworkPolicies with their expected selectors, the
// admission policy and binding for the namespace, the RuntimeClass of the
// tier, and the canary proof that the policies are enforced. The result
// is cached: a pass for PassTTL, a failure for FailureTTL, and either until
// a watch event on those objects (Start) or Invalidate. The error names the
// failed check.
func (i *Inspector) ClusterFacts(ctx context.Context) error {
	i.clusterMu.Lock()
	defer i.clusterMu.Unlock()
	now := i.now()
	if i.clusterDone {
		ttl := i.o.FailureTTL
		if i.clusterOK {
			ttl = i.o.PassTTL
		}
		if now.Sub(i.clusterAt) < ttl {
			return i.clusterErr
		}
	}
	err := i.proveCluster(ctx)
	i.clusterDone, i.clusterOK, i.clusterErr, i.clusterAt = true, err == nil, err, i.now()
	return err
}

// Invalidate forgets the cached cluster proof so the next ClusterFacts
// runs every check again. Watch events and the hourly timer call it.
func (i *Inspector) Invalidate() {
	i.clusterMu.Lock()
	defer i.clusterMu.Unlock()
	i.clusterDone = false
}

// proveCluster runs every check in order and returns the first failure.
func (i *Inspector) proveCluster(ctx context.Context) error {
	for _, check := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"sandbox namespace", i.checkNamespace},
		{"network policies", i.checkNetworkPolicies},
		{"admission policy", i.checkAdmission},
		{"runtime class", i.checkRuntimeClass},
		{"canary proof", i.checkCanaries},
	} {
		if err := check.fn(ctx); err != nil {
			return errors.New(check.name + ": " + err.Error())
		}
	}
	return nil
}

// checkNamespace: the namespace exists, is active, enforces PSA baseline
// (or stricter) and carries the Warden role label.
func (i *Inspector) checkNamespace(ctx context.Context) error {
	var ns api.Namespace
	if err := i.o.Client.Get(ctx, api.Namespaces, "", i.o.Namespace, &ns); err != nil {
		if api.IsNotFound(err) {
			return errors.New("namespace " + i.o.Namespace + " does not exist")
		}
		return err
	}
	if ns.Status.Phase != "" && ns.Status.Phase != "Active" {
		return errors.New("namespace " + i.o.Namespace + " is " + ns.Status.Phase)
	}
	switch ns.Metadata.Labels[PSAEnforceLabel] {
	case PSABaseline, "restricted":
	default:
		return errors.New("namespace " + i.o.Namespace + " does not enforce Pod Security " + PSABaseline)
	}
	if ns.Metadata.Labels[LabelRole] != RoleSandboxes {
		return errors.New("namespace " + i.o.Namespace + " lacks " + LabelRole + "=" + RoleSandboxes)
	}
	return nil
}

// checkNetworkPolicies: default-deny (both types, every pod), gateway-egress
// (the egress label to the policy pod in the core namespace on the gateway
// port and nothing else) and runner-ingress (every pod, from the runner in
// the core namespace only) exist with those shapes.
func (i *Inspector) checkNetworkPolicies(ctx context.Context) error {
	var list api.List[api.NetworkPolicy]
	if err := i.o.Client.List(ctx, api.NetworkPolicies, i.o.Namespace, api.ListOptions{}, &list); err != nil {
		return err
	}
	byName := map[string]*api.NetworkPolicy{}
	for idx := range list.Items {
		byName[list.Items[idx].Metadata.Name] = &list.Items[idx]
	}
	deny := byName[PolicyDefaultDeny]
	if deny == nil {
		return errors.New(PolicyDefaultDeny + " is missing")
	}
	if !emptySelector(deny.Spec.PodSelector) || !hasTypes(deny.Spec.PolicyTypes, "Ingress", "Egress") || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		return errors.New(PolicyDefaultDeny + " must deny ingress and egress for every pod")
	}
	egress := byName[PolicyGatewayEgress]
	if egress == nil {
		return errors.New(PolicyGatewayEgress + " is missing")
	}
	if err := i.checkGatewayEgress(egress); err != nil {
		return errors.New(PolicyGatewayEgress + " " + err.Error())
	}
	ingress := byName[PolicyRunnerIngress]
	if ingress == nil {
		return errors.New(PolicyRunnerIngress + " is missing")
	}
	if err := i.checkRunnerIngress(ingress); err != nil {
		return errors.New(PolicyRunnerIngress + " " + err.Error())
	}
	return nil
}

func (i *Inspector) checkGatewayEgress(np *api.NetworkPolicy) error {
	sel := np.Spec.PodSelector
	if len(sel.MatchExpressions) != 0 || len(sel.MatchLabels) != 1 || sel.MatchLabels[LabelEgress] != EgressGateway {
		return errors.New("must select exactly " + LabelEgress + "=" + EgressGateway)
	}
	if !sameTypes(np.Spec.PolicyTypes, "Egress") || len(np.Spec.Ingress) != 0 {
		return errors.New("must govern egress only")
	}
	if len(np.Spec.Egress) != 1 {
		return errors.New("must hold one egress rule")
	}
	rule := np.Spec.Egress[0]
	if len(rule.Ports) != 1 || !isTCPPort(rule.Ports[0], i.o.GatewayPort) {
		return fmt.Errorf("must allow TCP %d only", i.o.GatewayPort)
	}
	if len(rule.To) == 0 {
		return errors.New("must name the policy pod as its only peer")
	}
	for _, peer := range rule.To {
		if err := i.corePeer(peer, ComponentPolicy); err != nil {
			return err
		}
	}
	return nil
}

func (i *Inspector) checkRunnerIngress(np *api.NetworkPolicy) error {
	if !emptySelector(np.Spec.PodSelector) || !sameTypes(np.Spec.PolicyTypes, "Ingress") || len(np.Spec.Egress) != 0 {
		return errors.New("must govern ingress of every pod only")
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) == 0 {
		return errors.New("must hold one ingress rule naming the runner")
	}
	for _, port := range np.Spec.Ingress[0].Ports {
		if port.Protocol != nil && *port.Protocol != "TCP" {
			return errors.New("must admit TCP only")
		}
	}
	for _, peer := range np.Spec.Ingress[0].From {
		if err := i.corePeer(peer, ComponentRunner); err != nil {
			return err
		}
	}
	return nil
}

// corePeer requires a peer to be one component's pods in the core
// namespace: a namespace selector on the core namespace's name label and a
// pod selector carrying the component label, no ipBlock.
func (i *Inspector) corePeer(peer api.NetworkPolicyPeer, component string) error {
	if peer.IPBlock != nil || peer.NamespaceSelector == nil || peer.PodSelector == nil {
		return errors.New("peer must select pods of the core namespace")
	}
	if peer.NamespaceSelector.MatchLabels[NamespaceNameLabel] != i.o.CoreNamespace || len(peer.NamespaceSelector.MatchExpressions) != 0 {
		return errors.New("peer must select the core namespace " + i.o.CoreNamespace)
	}
	if peer.PodSelector.MatchLabels[LabelComponent] != component {
		return errors.New("peer must select the " + component + " pods")
	}
	return nil
}

// checkAdmission: a ValidatingAdmissionPolicy that fails closed on pod
// creation, requires the pinned runtimeClassName and the sandbox label,
// with a binding that denies and matches the sandbox namespace.
func (i *Inspector) checkAdmission(ctx context.Context) error {
	var bindings api.List[api.ValidatingAdmissionPolicyBinding]
	if err := i.o.Client.List(ctx, api.ValidatingAdmissionPolicyBindings, "", api.ListOptions{}, &bindings); err != nil {
		return err
	}
	var policies api.List[api.ValidatingAdmissionPolicy]
	if err := i.o.Client.List(ctx, api.ValidatingAdmissionPolicies, "", api.ListOptions{}, &policies); err != nil {
		return err
	}
	byName := map[string]*api.ValidatingAdmissionPolicy{}
	for idx := range policies.Items {
		byName[policies.Items[idx].Metadata.Name] = &policies.Items[idx]
	}
	var problems []string
	for _, b := range bindings.Items {
		if !bindsNamespace(b, i.o.Namespace) {
			continue
		}
		if !contains(b.Spec.ValidationActions, "Deny") {
			problems = append(problems, b.Metadata.Name+" does not deny")
			continue
		}
		p := byName[b.Spec.PolicyName]
		if p == nil {
			problems = append(problems, b.Metadata.Name+" binds a missing policy "+b.Spec.PolicyName)
			continue
		}
		if err := i.checkPolicyShape(p); err != nil {
			problems = append(problems, p.Metadata.Name+" "+err.Error())
			continue
		}
		return nil
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return errors.New("no ValidatingAdmissionPolicyBinding matches namespace " + i.o.Namespace)
}

// bindsNamespace reports whether the binding's match resources select the
// namespace by its name label (the chart's shape) or select every
// namespace.
func bindsNamespace(b api.ValidatingAdmissionPolicyBinding, namespace string) bool {
	if b.Spec.MatchResources == nil || b.Spec.MatchResources.NamespaceSelector == nil {
		return b.Spec.MatchResources == nil
	}
	return b.Spec.MatchResources.NamespaceSelector.Matches(map[string]string{NamespaceNameLabel: namespace})
}

// checkPolicyShape requires the policy to fail closed, to match pod
// creation, and to validate the runtimeClassName pin, the sandbox label,
// the token automount, the host namespaces, privilege and the volume
// types, by the text of its CEL expressions (the chart's).
func (i *Inspector) checkPolicyShape(p *api.ValidatingAdmissionPolicy) error {
	if p.Spec.FailurePolicy == nil || *p.Spec.FailurePolicy != "Fail" {
		return errors.New("must fail closed (failurePolicy Fail)")
	}
	matchesPods := false
	if p.Spec.MatchConstraints != nil {
		for _, rule := range p.Spec.MatchConstraints.ResourceRules {
			if contains(rule.Resources, "pods") && contains(rule.Operations, "CREATE") && (contains(rule.APIGroups, "") || contains(rule.APIGroups, "*")) {
				matchesPods = true
			}
		}
	}
	if !matchesPods {
		return errors.New("must match pod creation")
	}
	pinned := false
	all := ""
	for _, v := range p.Spec.Validations {
		all += v.Expression + "\n"
		if strings.Contains(v.Expression, "runtimeClassName") && strings.Contains(v.Expression, "== '"+i.o.RuntimeClass+"'") {
			pinned = true
		}
	}
	if !pinned {
		return errors.New("does not pin runtimeClassName " + i.o.RuntimeClass)
	}
	for _, r := range []struct{ needle, what string }{
		{"automountServiceAccountToken == false", "automountServiceAccountToken"},
		{"'" + LabelSandbox + "' in object.metadata.labels", "the sandbox label"},
		{"hostNetwork", "host namespaces"},
		{"privileged", "privileged containers"},
		{"has(v.persistentVolumeClaim)", "volume types"},
	} {
		if !strings.Contains(all, r.needle) {
			return errors.New("does not validate " + r.what)
		}
	}
	return nil
}

// checkRuntimeClass: the pinned RuntimeClass exists and its handler is in
// the tier's allow-list.
func (i *Inspector) checkRuntimeClass(ctx context.Context) error {
	var rc api.RuntimeClass
	if err := i.o.Client.Get(ctx, api.RuntimeClasses, "", i.o.RuntimeClass, &rc); err != nil {
		if api.IsNotFound(err) {
			return errors.New("RuntimeClass " + i.o.RuntimeClass + " does not exist")
		}
		return err
	}
	if !contains(i.handlers, rc.Handler) {
		return fmt.Errorf("RuntimeClass %s handler %q is not a %s handler (%s)", i.o.RuntimeClass, rc.Handler, i.o.Tier, strings.Join(i.handlers, ", "))
	}
	return nil
}

// checkCanaries runs the canary proof (canary.go) with its own bound.
func (i *Inspector) checkCanaries(ctx context.Context) error {
	if i.o.Canary.Disabled {
		return nil
	}
	timeout := i.o.Canary.Timeout
	if timeout <= 0 {
		timeout = DefaultCanaryTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	i.stopMu.Lock()
	stopCtx := i.stopCtx
	i.stopMu.Unlock()
	if stopCtx != nil {
		defer context.AfterFunc(stopCtx, cancel)()
	}
	return i.runCanaries(ctx)
}

func emptySelector(s api.LabelSelector) bool {
	return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}

func hasTypes(types []string, want ...string) bool {
	for _, w := range want {
		if !contains(types, w) {
			return false
		}
	}
	return true
}

func sameTypes(types []string, want ...string) bool {
	return len(types) == len(want) && hasTypes(types, want...)
}

func isTCPPort(p api.NetworkPolicyPort, port int) bool {
	if p.Protocol != nil && *p.Protocol != "TCP" {
		return false
	}
	return p.Port != nil && !p.Port.IsString && int(p.Port.IntVal) == port && p.EndPort == nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// DefaultCanaryTimeout bounds one canary proof: two pod starts, four
// three-second probes each and the log reads.
const DefaultCanaryTimeout = 3 * time.Minute
