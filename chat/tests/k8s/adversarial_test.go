//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/kube"
)

// The adversarial networking rows of docs/sbx-integration-plan.md as the
// Kubernetes shape runs them (docs/warden-kubernetes.md, "Threat-model
// delta"): the suite, not the verifier, execs into a running sandbox pod
// as the guest account and tries every escape with the tools the guest
// image ships (curl, python3). Each row asserts its expected refusal and
// a positive control where one exists, so nothing passes by silence: a
// probe that could not run is a failure, not a block.
//
// The gateway serves a binding only while its run holds a lease; both
// agents are resident between turns, so the lease is live right after a
// turn and the rows run against the real destination policy, not against
// an absent lease. The positive control at the start proves it.
func (h *harness) adversarial(t *testing.T) {
	a := h.chat(t, "codex", "codex")
	proxyA := h.proxyURL(t, a)
	podA := h.pod(t, a)
	gateway := proxyA.Host
	t.Logf("pod A %s, gateway %s (Service %s)", describePod(podA), gateway, h.cfg.Kubernetes.GatewayService)
	if got := h.gatewayAnswers(t, podA.Metadata.Name, proxyA.String()); got.Connect != 200 || got.Code != 200 {
		t.Fatalf("positive control: an approved dependency GET through the gateway did not succeed from %s: %s", podA.Metadata.Name, got)
	}
	t.Run("DirectEgress", func(t *testing.T) { h.rowDirectEgress(t, podA, proxyA) })
	t.Run("ProxyCredentials", func(t *testing.T) { h.rowProxyCredentials(t, podA, gateway) })
	t.Run("IPLiteral", func(t *testing.T) { h.rowIPLiteral(t, podA, proxyA) })
	t.Run("IPv6", func(t *testing.T) { h.rowIPv6(t, podA, proxyA) })
	t.Run("DNS", func(t *testing.T) { h.rowDNS(t, podA, proxyA) })
	t.Run("ConnectNonHTTP", func(t *testing.T) { h.rowConnectNonHTTP(t, podA, proxyA) })
	t.Run("TrustBundle", func(t *testing.T) { h.rowTrustBundle(t, podA, proxyA) })
	t.Run("ClusterAddresses", func(t *testing.T) { h.rowClusterAddresses(t, podA) })
	t.Run("OtherBindingCredential", func(t *testing.T) { h.rowOtherBinding(t, a, podA) })
	t.Run("OtherSandboxPreview", func(t *testing.T) { h.rowOtherSandboxPreview(t, a, podA) })
	t.Run("UnlabelledPod", func(t *testing.T) { h.rowUnlabelledPod(t, gateway) })
}

// gatewayAnswers is the positive control: a GET to an approved dependency
// host through the binding's gateway.
func (h *harness) gatewayAnswers(t *testing.T, pod, proxy string) curlResult {
	t.Helper()
	return h.curl(t, pod, "-x '"+proxy+"' https://registry.npmjs.org/-/ping")
}

// blocked reports whether a curl probe made no connection at all: no
// proxy answer, no status, a non-zero exit.
func blocked(r curlResult) bool {
	return r.Exit != 0 && r.Connect == 0 && r.Code == 0
}

// Direct egress with the proxy variables unset (the pod's own environment
// has none; the runner sets them for the agent process only), with
// no_proxy=*, and with the gateway address as an ordinary destination.
func (h *harness) rowDirectEgress(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	env := h.exec(t, name, "env | grep -ci proxy || true")
	if strings.TrimSpace(env.Stdout) != "0" {
		h.record(t, "direct-egress", pod, false, "the exec environment carries proxy variables: "+clip(env.Stdout, 100))
		return
	}
	ip := h.curl(t, name, "--noproxy '*' --max-time 8 https://1.1.1.1/")
	dns := h.curl(t, name, "--noproxy '*' --max-time 8 https://example.com/")
	noProxy := h.curl(t, name, "--max-time 8 https://example.com/")
	starred := h.exec(t, name, "HTTPS_PROXY='"+proxy.String()+"' no_proxy='*' curl -sS -o /dev/null --max-time 8 https://example.com/ 2>&1; echo \"exit=$?\"")
	plain := h.curl(t, name, "--noproxy '*' --max-time 8 http://93.184.216.34/")
	ok := blocked(ip) && blocked(dns) && blocked(noProxy) && blocked(plain) &&
		strings.Contains(starred.Stdout, "exit=6") // could not resolve host
	h.record(t, "direct-egress", pod, ok, fmt.Sprintf("1.1.1.1:443 [%s]; example.com [%s]; no proxy env [%s]; no_proxy=* [%s]; http 93.184.216.34 [%s]",
		ip, dns, noProxy, clip(starred.Stdout, 120), plain))
}

// Missing and wrong proxy credentials are challenged with 407 and
// Proxy-Authenticate, and nothing upstream is contacted.
func (h *harness) rowProxyCredentials(t *testing.T, pod *kube.Pod, gateway string) {
	name := pod.Metadata.Name
	none := h.curl(t, name, "-x 'http://"+gateway+"' https://registry.npmjs.org/-/ping")
	wrong := h.curl(t, name, "-x 'http://wrong:credential@"+gateway+"' https://registry.npmjs.org/-/ping")
	// A bearer that is not a binding's credential on the provider route.
	bearer := h.curl(t, name, "--noproxy '*' -H 'Authorization: Bearer nobody.nothing' http://"+gateway+"/openai/v1/models")
	headers := h.exec(t, name, "curl -sS -o /dev/null -D - --max-time 20 -x 'http://"+gateway+"' https://registry.npmjs.org/-/ping 2>/dev/null | tr -d '\\r' | grep -i '^Proxy-Authenticate:' || true")
	ok := none.Connect == 407 && none.Code == 0 && wrong.Connect == 407 && wrong.Code == 0 && bearer.Code == 401 &&
		strings.Contains(strings.ToLower(headers.Stdout), "basic")
	h.record(t, "proxy-credentials", pod, ok, fmt.Sprintf("no credential [%s]; wrong credential [%s]; bearer on provider route [%s]; challenge %q", none, wrong, bearer, strings.TrimSpace(headers.Stdout)))
}

// IP literals are refused at the gateway (destinations are DNS names the
// policy resolves) and unreachable directly.
func (h *harness) rowIPLiteral(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	p := "-x '" + proxy.String() + "' "
	https := h.curl(t, name, p+"https://1.1.1.1/")
	http := h.curl(t, name, p+"http://1.1.1.1/")
	gatewayIP, _, _ := net.SplitHostPort(proxy.Host)
	self := h.curl(t, name, p+"https://"+gatewayIP+"/")
	alt := h.curl(t, name, p+"https://registry.npmjs.org:8443/")
	direct := h.curl(t, name, "--noproxy '*' --max-time 8 http://1.1.1.1:80/")
	ok := https.Connect == 403 && https.Code == 0 && http.Code == 403 && self.Connect == 403 && alt.Connect == 403 && blocked(direct)
	h.record(t, "ip-literal", pod, ok, fmt.Sprintf("CONNECT 1.1.1.1:443 [%s]; http://1.1.1.1 [%s]; CONNECT gateway IP [%s]; allowed host on 8443 [%s]; direct [%s]", https, http, self, alt, direct))
}

// IPv6: the pod has no global address, literals fail directly and are
// refused by the gateway.
func (h *harness) rowIPv6(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	addrs := h.exec(t, name, "cat /proc/net/if_inet6 2>/dev/null || echo none")
	global := 0
	for _, line := range strings.Split(strings.TrimSpace(addrs.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || len(f[0]) != 32 {
			continue
		}
		// The address is 32 hex digits; ::1 and fe80::/10 are the only
		// ones a pod without IPv6 routing has (gVisor reports every scope
		// as 00, so the prefix is what to look at).
		loopback := strings.TrimLeft(f[0], "0") == "1"
		linkLocal := strings.HasPrefix(f[0], "fe8") || strings.HasPrefix(f[0], "fe9") || strings.HasPrefix(f[0], "fea") || strings.HasPrefix(f[0], "feb")
		if !loopback && !linkLocal {
			global++
		}
	}
	direct := h.curl(t, name, "--noproxy '*' --max-time 8 'https://[2606:4700:4700::1111]/'")
	viaProxy := h.curl(t, name, "-x '"+proxy.String()+"' 'https://[2606:4700:4700::1111]/'")
	mapped := h.curl(t, name, "--noproxy '*' --max-time 8 'http://[::ffff:1.1.1.1]/'")
	ok := global == 0 && blocked(direct) && viaProxy.Connect == 403 && blocked(mapped)
	h.record(t, "ipv6", pod, ok, fmt.Sprintf("global addresses %d; direct literal [%s]; via proxy [%s]; mapped [%s]", global, direct, viaProxy, mapped))
}

// pythonProbe is a TCP or UDP connect attempt with a timeout, printing
// "connected <target>" or "blocked <target> <error>".
const pythonProbe = `python3 - "$@" <<'PY'
import socket, sys
for spec in sys.argv[1:]:
    proto, host, port = spec.split(":", 2)
    port = int(port)
    try:
        if proto == "udp":
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
            q = bytes.fromhex("1234010000010000000000000765786d706c6503636f6d0000010001")
            s.sendto(q, (host, port)); data, _ = s.recvfrom(512)
            print("connected", spec, data[:4].hex())
        else:
            s = socket.create_connection((host, port), timeout=3)
            print("connected", spec)
        s.close()
    except Exception as e:
        print("blocked", spec, type(e).__name__)
PY
`

// probeTargets runs pythonProbe against every "proto:host:port" and
// returns the output; every target must be blocked and every target must
// have been probed.
func (h *harness) probeTargets(t *testing.T, pod string, targets ...string) (string, bool) {
	t.Helper()
	res := h.exec(t, pod, "set -- "+strings.Join(targets, " ")+"\n"+pythonProbe)
	out := strings.TrimSpace(res.Stdout)
	ok := res.Code == 0 && strings.Count(out, "blocked ") == len(targets) && !strings.Contains(out, "connected ")
	return out + strings.TrimSpace(" "+res.Stderr), ok
}

// DNS: the cluster resolver and a public resolver on 53 (UDP and TCP), DoT
// on 853, the system resolver, and DNS over HTTPS through the gateway.
func (h *harness) rowDNS(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	resolver := strings.TrimSpace(h.exec(t, name, "awk '/^nameserver/{print $2; exit}' /etc/resolv.conf").Stdout)
	if net.ParseIP(resolver) == nil {
		h.record(t, "dns", pod, false, "no nameserver in the pod's resolv.conf: "+resolver)
		return
	}
	out, ok := h.probeTargets(t, name, "udp:"+resolver+":53", "tcp:"+resolver+":53", "udp:1.1.1.1:53", "tcp:1.1.1.1:53", "tcp:1.1.1.1:853", "tcp:8.8.8.8:53")
	getent := h.exec(t, name, "getent hosts example.com; echo \"exit=$?\"")
	doh := h.curl(t, name, "-x '"+proxy.String()+"' 'https://cloudflare-dns.com/dns-query?name=example.com&type=A' -H 'accept: application/dns-json'")
	dohDirect := h.curl(t, name, "--noproxy '*' --max-time 8 'https://cloudflare-dns.com/dns-query?name=example.com&type=A'")
	resolved := strings.Contains(getent.Stdout, "exit=0") || strings.Contains(getent.Stdout, "example.com")
	ok = ok && !resolved && doh.Connect == 403 && doh.Code == 0 && blocked(dohDirect)
	h.record(t, "dns", pod, ok, fmt.Sprintf("resolver %s; probes: %s; getent %q; DoH via gateway [%s]; DoH direct [%s]", resolver, strings.ReplaceAll(out, "\n", ", "), strings.TrimSpace(getent.Stdout), doh, dohDirect))
}

// CONNECT to anything but 443 on an allowed host is refused, a plain-HTTP
// tunnel too, and bytes that are not HTTP on the gateway port are not
// forwarded anywhere.
func (h *harness) rowConnectNonHTTP(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	p := "-x '" + proxy.String() + "' "
	ssh := h.curl(t, name, p+"https://registry.npmjs.org:22/")
	tunnel80 := h.curl(t, name, p+"--proxytunnel http://registry.npmjs.org/-/ping")
	post := h.curl(t, name, p+"-X POST -d x=1 https://registry.npmjs.org/-/ping")
	gatewayHost, gatewayPort, _ := net.SplitHostPort(proxy.Host)
	raw := h.exec(t, name, `python3 - `+gatewayHost+` `+gatewayPort+` <<'PY'
import socket, sys
s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5)
s.sendall(b"SSH-2.0-OpenSSH_9.9\r\n")
try:
    print("reply", s.recv(200)[:60])
except Exception as e:
    print("noreply", type(e).__name__)
PY
`)
	rawOK := strings.Contains(raw.Stdout, "400 Bad Request") || strings.Contains(raw.Stdout, "noreply")
	ok := ssh.Connect == 403 && ssh.Code == 0 && tunnel80.Connect == 403 && tunnel80.Code == 0 && post.Connect == 200 && post.Code == 403 && rawOK
	h.record(t, "connect-non-http", pod, ok, fmt.Sprintf("CONNECT :22 [%s]; tunnel to :80 [%s]; POST on a GET-only host [%s]; SSH banner on the gateway port: %s", ssh, tunnel80, post, clip(raw.Stdout, 120)))
}

// Dropping the gateway CA from the trust bundle makes TLS through the
// gateway fail, and nothing falls back to a direct connection. The bundle
// the guest trusts is the mounted ConfigMap: the system CAs with the
// gateway CA appended last.
func (h *harness) rowTrustBundle(t *testing.T, pod *kube.Pod, proxy *url.URL) {
	name := pod.Metadata.Name
	p := "-x '" + proxy.String() + "' "
	prep := h.exec(t, name, `n=$(grep -c 'BEGIN CERTIFICATE' /opt/warden/trust/ca-certificates.crt); awk -v n="$n" '/BEGIN CERTIFICATE/{c++} c<n{print}' /opt/warden/trust/ca-certificates.crt > /tmp/no-gateway-ca.pem; echo "$n $(grep -c 'BEGIN CERTIFICATE' /tmp/no-gateway-ca.pem)"; readlink -f /etc/ssl/certs/ca-certificates.crt`)
	counts := strings.Fields(prep.Stdout)
	bundleOK := len(counts) == 3 && atoi(counts[0]) > 1 && atoi(counts[1]) == atoi(counts[0])-1 && strings.HasPrefix(counts[2], "/opt/warden/trust/")
	full := h.curl(t, name, p+"--cacert /opt/warden/trust/ca-certificates.crt https://registry.npmjs.org/-/ping")
	without := h.curl(t, name, p+"--cacert /tmp/no-gateway-ca.pem https://registry.npmjs.org/-/ping")
	none := h.curl(t, name, p+"--cacert /dev/null https://registry.npmjs.org/-/ping")
	unset := h.exec(t, name, "env -u CURL_CA_BUNDLE -u SSL_CERT_FILE -u SSL_CERT_DIR curl -sS -o /dev/null --max-time 20 --cacert /tmp/no-gateway-ca.pem -x '"+proxy.String()+"' https://registry.npmjs.org/-/ping 2>&1; echo \"exit=$?\"")
	ok := bundleOK &&
		full.Connect == 200 && full.Code == 200 &&
		without.Connect == 200 && without.Code == 0 && without.Exit == 60 &&
		none.Exit != 0 && none.Code != 200 && strings.Contains(unset.Stdout, "exit=60")
	h.record(t, "trust-bundle", pod, ok, fmt.Sprintf("bundle [%s]; full bundle [%s]; without the gateway CA [%s]; /dev/null [%s]; env cleared: %s",
		strings.Join(counts, " "), full, without, none, clip(strings.ReplaceAll(unset.Stdout, "\n", " "), 160)))
}

// The API server, cluster DNS, the metadata address, the node, and the
// release's control and preview ports are unreachable from a sandbox.
func (h *harness) rowClusterAddresses(t *testing.T, pod *kube.Pod) {
	name := pod.Metadata.Name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var targets []string
	var kubeSvc kube.Object
	if err := h.kube.Get(ctx, services, "default", "kubernetes", &kubeSvc); err == nil {
		if ip := serviceIP(kubeSvc); ip != "" {
			targets = append(targets, "tcp:"+ip+":443")
		}
	}
	for _, svc := range []struct {
		name  string
		ports []string
	}{{"warden-policy", []string{"7443"}}, {"warden-runner", []string{"7444", "7446"}}, {"warden-chat", []string{"7445"}}, {"warden-edge", nil}} {
		var obj kube.Object
		if err := h.kube.Get(ctx, services, h.s.namespace, svc.name, &obj); err != nil {
			continue
		}
		ip := serviceIP(obj)
		if ip == "" {
			continue
		}
		ports := svc.ports
		if ports == nil {
			ports = servicePorts(obj)
		}
		for _, p := range ports {
			targets = append(targets, "tcp:"+ip+":"+p)
		}
	}
	targets = append(targets, "tcp:169.254.169.254:80", "tcp:10.0.0.1:443")
	if pod.Status.HostIP != "" {
		targets = append(targets, "tcp:"+pod.Status.HostIP+":6443", "tcp:"+pod.Status.HostIP+":10250", "tcp:"+pod.Status.HostIP+":22")
	}
	// The policy pod on a port other than the gateway's.
	policy := h.componentPod(t, "policy")
	if policy.Status.PodIP != "" {
		targets = append(targets, "tcp:"+policy.Status.PodIP+":7443")
	}
	out, ok := h.probeTargets(t, name, targets...)
	h.record(t, "cluster-addresses", pod, ok && len(targets) >= 6, fmt.Sprintf("%d targets: %s", len(targets), strings.ReplaceAll(out, "\n", ", ")))
}

func serviceIP(obj kube.Object) string {
	spec, _ := obj["spec"].(map[string]any)
	ip, _ := spec["clusterIP"].(string)
	if ip == "None" {
		return ""
	}
	return ip
}

func servicePorts(obj kube.Object) []string {
	spec, _ := obj["spec"].(map[string]any)
	list, _ := spec["ports"].([]any)
	var out []string
	for _, item := range list {
		m, _ := item.(map[string]any)
		if p, ok := m["port"].(float64); ok {
			out = append(out, strconv.Itoa(int(p)))
		}
	}
	return out
}

// Another binding's credential presented from this pod must be refused:
// the credential identifies the binding and the source pod is the second
// check (plan decision 4, "the source pod … is a second check"). Chat B's
// credential works from B's own pod (the positive control) and must not
// work from A. This needs A and B running at once; when the shared cluster
// cannot give a second slot the row is recorded as a capacity failure, not
// skipped.
func (h *harness) rowOtherBinding(t *testing.T, a *suiteChat, podA *kube.Pod) {
	b, reason := h.secondSandbox(t, "claude", "claude", a)
	if reason != "" {
		h.record(t, "other-binding-credential", podA, false, "could not start a second suite sandbox to hold another binding: "+reason)
		return
	}
	proxyB := h.proxyURL(t, b, a)
	podB := h.pod(t, b)
	fromB := h.gatewayAnswers(t, podB.Metadata.Name, proxyB.String())
	if fromB.Connect != 200 || fromB.Code != 200 {
		h.record(t, "other-binding-credential", podA, false, "positive control: B's credential does not work from B's own pod "+podB.Metadata.Name+": "+fromB.String())
		return
	}
	// A's own credential still works from A (the run's lease is live), so a
	// refusal below is B's credential being rejected from A, not a dead
	// gateway.
	proxyA := h.proxyURL(t, a, b)
	if own := h.gatewayAnswers(t, podA.Metadata.Name, proxyA.String()); own.Connect != 200 || own.Code != 200 {
		h.record(t, "other-binding-credential", podA, false, "positive control: A's own credential does not work from A: "+own.String())
		return
	}
	fromA := h.gatewayAnswers(t, podA.Metadata.Name, proxyB.String())
	bearer := h.curl(t, podA.Metadata.Name, "--noproxy '*' -H 'Authorization: Bearer "+proxyB.User.Username()+"."+passwordOf(proxyB)+"' http://"+proxyB.Host+"/openai/v1/models")
	ok := fromA.Connect != 200 && fromA.Code != 200 && bearer.Code != 200
	detail := fmt.Sprintf("B's credential from B %s [%s]; B's credential from A %s [%s]; B's bearer on the provider route from A [%s]", podB.Metadata.Name, fromB, podA.Metadata.Name, fromA, bearer)
	if !ok {
		// The CONNECT/proxy path authenticates by the binding credential
		// alone: the gateway served pod A when it presented pod B's
		// credential, so the source-pod "second check" of plan decision 4 is
		// not enforced in this build (the credential is the sole authority).
		// Not exploitable on its own — a guest cannot obtain another pod's
		// credential (per-pod delivery, default-deny isolation, proven by the
		// other rows) — but the documented defence-in-depth layer is absent.
		detail = "PRODUCT GAP: the gateway has no source-pod check; a pod presenting another binding's valid credential is served. " + detail
	}
	h.record(t, "other-binding-credential", podA, ok, detail)
}

func passwordOf(u *url.URL) string {
	p, _ := u.User.Password()
	return p
}

// Another sandbox's preview port: a listener in pod B is reachable by the
// runner (the preview flow proved that) and by nobody else in the
// namespace. With a second suite sandbox the check is two-directional; when
// the cluster cannot give one, it falls back to a one-directional probe
// from pod A to any other running sandbox's pod (read-only, so it needs no
// second slot and touches no other chat), which still asserts that the
// default-deny NetworkPolicy drops sandbox-to-sandbox traffic.
func (h *harness) rowOtherSandboxPreview(t *testing.T, a *suiteChat, podA *kube.Pod) {
	b, reason := h.secondSandbox(t, "claude", "claude", a)
	if reason == "" {
		podB := h.pod(t, b)
		up := h.exec(t, podB.Metadata.Name, "cd /tmp && (nohup python3 -m http.server 8081 --bind 0.0.0.0 >/tmp/k8s-suite-8081.log 2>&1 &) ; sleep 2; curl -sS -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:8081/")
		defer h.exec(t, podB.Metadata.Name, "pkill -f 'http.server 8081' || true")
		if strings.TrimSpace(up.Stdout) != "200" {
			h.record(t, "other-sandbox-preview", podA, false, "could not start a listener in "+podB.Metadata.Name+": "+clip(up.Stdout+up.Stderr, 200))
			return
		}
		fromA := h.curl(t, podA.Metadata.Name, "--noproxy '*' --max-time 8 http://"+podB.Status.PodIP+":8081/")
		fromB := h.curl(t, podB.Metadata.Name, "--noproxy '*' --max-time 8 http://"+podA.Status.PodIP+":8080/")
		out, probed := h.probeTargets(t, podA.Metadata.Name, "tcp:"+podB.Status.PodIP+":8081", "tcp:"+podB.Status.PodIP+":22")
		ok := blocked(fromA) && blocked(fromB) && probed
		h.record(t, "other-sandbox-preview", podA, ok, fmt.Sprintf("A -> B %s:8081 [%s]; B -> A %s:8080 [%s]; %s", podB.Status.PodIP, fromA, podA.Status.PodIP, fromB, strings.ReplaceAll(out, "\n", ", ")))
		return
	}
	// Fallback: probe another running sandbox's pod from A only.
	other := h.otherRunningSandboxPod(t, a)
	if other == nil {
		h.record(t, "other-sandbox-preview", podA, false, "no second sandbox available and no other running sandbox pod to probe ("+reason+")")
		return
	}
	fromA := h.curl(t, podA.Metadata.Name, "--noproxy '*' --max-time 8 http://"+other.Status.PodIP+":8080/")
	out, probed := h.probeTargets(t, podA.Metadata.Name, "tcp:"+other.Status.PodIP+":8080", "tcp:"+other.Status.PodIP+":22")
	ok := blocked(fromA) && probed
	h.record(t, "other-sandbox-preview", podA, ok, fmt.Sprintf("second slot unavailable (%s); one-directional from A to another running sandbox pod %s: http :8080 [%s]; %s", reason, other.Metadata.Name, fromA, strings.ReplaceAll(out, "\n", ", ")))
}

// otherRunningSandboxPod returns a running sandbox pod that is not c's, for
// the read-only isolation fallback. It never returns a canary or the
// suite's own second-sandbox pod.
func (h *harness) otherRunningSandboxPod(t *testing.T, c *suiteChat) *kube.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mine := h.pod(t, c)
	var list kube.List[kube.Pod]
	if err := h.kube.List(ctx, kube.Pods, h.s.sandboxNS, kube.ListOptions{LabelSelector: labelSandbox}, &list); err != nil {
		t.Fatalf("listing sandbox pods: %v", err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.Metadata.Name == mine.Metadata.Name || p.Status.Phase != "Running" || p.Status.PodIP == "" {
			continue
		}
		if _, canary := p.Metadata.Labels["warden.monaddle.com/canary"]; canary {
			continue
		}
		if p.Metadata.Labels[labelManagedBy] == managedBySuite {
			continue
		}
		return p
	}
	return nil
}

// A pod in the sandbox namespace without the egress label reaches nothing,
// not even the gateway; and the admission policy refuses a pod outside the
// RuntimeClass. The pod is the suite's own (a distinct managed-by value,
// so the runner's reconcile ignores it) and is deleted at once.
func (h *harness) rowUnlabelledPod(t *testing.T, gateway string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	name := "wk-suite-nolabel-" + strings.ToLower(time.Now().Format("150405"))
	image := h.cfg.Kubernetes.GuestImage + "@" + h.cfg.Kubernetes.GuestImageDigest
	spec := func(class *string) *kube.Pod {
		return &kube.Pod{
			Metadata: kube.ObjectMeta{Name: name, Namespace: h.s.sandboxNS, Labels: map[string]string{labelSandbox: name, labelManagedBy: managedBySuite}},
			Spec: kube.PodSpec{
				RuntimeClassName:              class,
				AutomountServiceAccountToken:  kube.Bool(false),
				RestartPolicy:                 "Never",
				TerminationGracePeriodSeconds: kube.Int64(1),
				EnableServiceLinks:            kube.Bool(false),
				SecurityContext:               &kube.PodSecurityContext{RunAsUser: kube.Int64(1000), RunAsGroup: kube.Int64(1000), RunAsNonRoot: kube.Bool(true), SeccompProfile: &kube.SeccompProfile{Type: "RuntimeDefault"}},
				Containers: []kube.Container{{
					Name:  guestContainer,
					Image: image,
					Args:  []string{"sleep", "600"},
					Resources: kube.ResourceRequirements{
						Requests: kube.ResourceList{"cpu": "100m", "memory": "256Mi"},
						Limits:   kube.ResourceList{"cpu": "100m", "memory": "256Mi"},
					},
					SecurityContext: &kube.SecurityContext{Capabilities: &kube.Capabilities{Drop: []string{"ALL"}}},
				}},
			},
		}
	}
	// Admission first: no RuntimeClass, no pod.
	var refused kube.Pod
	err := h.kube.Create(ctx, kube.Pods, h.s.sandboxNS, spec(nil), &refused)
	admission := err != nil && (kube.IsForbidden(err) || strings.Contains(err.Error(), "runtimeClassName"))
	if err == nil {
		h.trackPod(name)
	}
	detail := fmt.Sprintf("without runtimeClassName: %v", err)
	if !admission {
		h.record(t, "unlabelled-pod", nil, false, "the admission policy admitted a pod without the RuntimeClass: "+detail)
		return
	}
	var created kube.Pod
	if err := h.kube.Create(ctx, kube.Pods, h.s.sandboxNS, spec(kube.String(h.cfg.Kubernetes.RuntimeClass)), &created); err != nil {
		h.record(t, "unlabelled-pod", nil, false, "creating the suite's pod: "+err.Error())
		return
	}
	h.trackPod(name)
	defer func() {
		_ = h.kube.Delete(context.Background(), kube.Pods, h.s.sandboxNS, name, kube.DeleteOptions{GracePeriodSeconds: kube.Int64(0)})
	}()
	var pod kube.Pod
	for {
		if err := h.kube.Get(ctx, kube.Pods, h.s.sandboxNS, name, &pod); err != nil {
			h.record(t, "unlabelled-pod", nil, false, err.Error())
			return
		}
		if pod.Status.Phase == "Running" && pod.Status.PodIP != "" {
			break
		}
		if pod.Status.Phase == "Failed" || ctx.Err() != nil {
			h.record(t, "unlabelled-pod", &pod, false, fmt.Sprintf("pod %s is %s: %s", name, pod.Status.Phase, pod.Status.Message))
			return
		}
		time.Sleep(2 * time.Second)
	}
	if _, labelled := pod.Metadata.Labels[labelEgress]; labelled {
		h.record(t, "unlabelled-pod", &pod, false, "the pod gained an egress label nobody granted")
		return
	}
	resolver := strings.TrimSpace(h.exec(t, name, "awk '/^nameserver/{print $2; exit}' /etc/resolv.conf").Stdout)
	targets := []string{"tcp:" + gateway, "tcp:1.1.1.1:443", "udp:1.1.1.1:53"}
	if net.ParseIP(resolver) != nil {
		targets = append(targets, "udp:"+resolver+":53")
	}
	out, ok := h.probeTargets(t, name, targets...)
	gw := h.curl(t, name, "-x 'http://"+gateway+"' https://registry.npmjs.org/-/ping")
	ok = ok && blocked(gw)
	h.record(t, "unlabelled-pod", &pod, ok, fmt.Sprintf("%s; %s; gateway as proxy [%s]", detail, strings.ReplaceAll(out, "\n", ", "), gw))
}
