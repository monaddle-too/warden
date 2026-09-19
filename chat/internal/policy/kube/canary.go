package kube

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	api "warden/chat/internal/kube"
)

// CanaryOptions shape the canary proof of decision 12.
type CanaryOptions struct {
	// Disabled skips the proof (tests of the other checks only; never in
	// production).
	Disabled bool
	// Image is the canary image; DefaultCanaryImage when empty.
	Image string
	// Timeout bounds one proof; DefaultCanaryTimeout when zero.
	Timeout time.Duration
	// GatewayHost is the address the labelled canary must reach on the
	// gateway port; Inspector.SetGateway overrides it.
	GatewayHost string
	// APIServer is the API server's host:port as pods reach it; empty means
	// KUBERNETES_SERVICE_HOST:KUBERNETES_SERVICE_PORT, else the client's
	// host.
	APIServer string
	// External is the external host:port both canaries must fail to reach;
	// DefaultExternal when empty.
	External string
	// PollInterval is how often a canary's phase is read; two seconds when
	// zero.
	PollInterval time.Duration
	// PriorityClass is the canaries' PriorityClass: the warm spares' (a
	// negative value that never preempts), so a canary that finds no room
	// waits for a node instead of evicting a spare, whose replacement
	// would then boot a node for nothing at every policy restart. None
	// when empty.
	PriorityClass string
}

const (
	// DefaultCanaryImage is the canary image: busybox, whose nc has -z and
	// -w (the step 0 spike used the same).
	DefaultCanaryImage = "docker.io/library/busybox:1.37"
	// DefaultExternal is the external address a canary must not reach.
	DefaultExternal = "1.1.1.1:443"
	// CanaryDeny and CanaryGateway are the two roles (LabelCanary values).
	CanaryDeny    = "deny"
	CanaryGateway = "gateway"
	// canaryPrefix is the line each probe prints.
	canaryPrefix = "warden-canary "
)

// The probes each canary runs, by name, in the order the script prints them.
var canaryProbes = []string{"gateway", "apiserver", "dns", "external"}

// canaryScript is the canary's command: one nc -z -w 3 per target, one
// line per result. The cluster DNS address is the pod's own resolver.
const canaryScript = `probe() { if nc -z -w 3 "$1" "$2" >/dev/null 2>&1; then echo "warden-canary $3 open"; else echo "warden-canary $3 closed"; fi; }
# The gateway is the one destination a labelled canary must reach; its
# Service endpoint can lag a policy restart by a few seconds, so that probe
# alone is retried. Denials are never retried into a pass.
gateway_probe() { n=0; while [ $n -lt "${GATEWAY_TRIES:-1}" ]; do if nc -z -w 3 "$GATEWAY_HOST" "$GATEWAY_PORT" >/dev/null 2>&1; then echo "warden-canary gateway open"; return; fi; n=$((n+1)); sleep 2; done; echo "warden-canary gateway closed"; }
dns=$(awk '/^nameserver/{print $2; exit}' /etc/resolv.conf 2>/dev/null)
gateway_probe
probe "$API_HOST" "$API_PORT" apiserver
if [ -n "$dns" ]; then probe "$dns" 53 dns; else echo "warden-canary dns unknown"; fi
probe "$EXTERNAL_HOST" "$EXTERNAL_PORT" external
echo "warden-canary done"
`

// runCanaries creates the two canaries, waits for both to finish, reads
// their verdicts from pods/log and deletes them. Both run at once.
func (i *Inspector) runCanaries(ctx context.Context) error {
	gateway := i.gateway()
	if gateway == "" {
		return errors.New("the gateway address is unknown")
	}
	apiHost, apiPort, err := i.apiServer()
	if err != nil {
		return err
	}
	externalHost, externalPort, err := splitHostPort(i.o.Canary.External, DefaultExternal)
	if err != nil {
		return errors.New("external address: " + err.Error())
	}
	env := []api.EnvVar{
		{Name: "GATEWAY_HOST", Value: gateway}, {Name: "GATEWAY_PORT", Value: strconv.Itoa(i.o.GatewayPort)},
		{Name: "API_HOST", Value: apiHost}, {Name: "API_PORT", Value: apiPort},
		{Name: "EXTERNAL_HOST", Value: externalHost}, {Name: "EXTERNAL_PORT", Value: externalPort},
	}
	i.sweepCanaries(ctx)
	suffix := randomSuffix()
	type run struct {
		role string
		pod  *api.Pod
		out  map[string]string
		err  error
	}
	runs := []*run{{role: CanaryDeny}, {role: CanaryGateway}}
	for _, r := range runs {
		r.pod = i.canarySpec(r.role, suffix, append(append([]api.EnvVar{}, env...), api.EnvVar{Name: "GATEWAY_TRIES", Value: gatewayTries(r.role)}))
	}
	// Delete whatever was created, on every path.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, r := range runs {
			_ = i.o.Client.Delete(cleanup, api.Pods, i.o.Namespace, r.pod.Metadata.Name, api.DeleteOptions{GracePeriodSeconds: api.Int64(0)})
		}
	}()
	for _, r := range runs {
		var created api.Pod
		if err := i.o.Client.Create(ctx, api.Pods, i.o.Namespace, r.pod, &created); err != nil {
			return fmt.Errorf("canary %s could not be created: %w", r.role, err)
		}
	}
	done := make(chan *run, len(runs))
	for _, r := range runs {
		go func(r *run) {
			r.out, r.err = i.awaitCanary(ctx, r.pod.Metadata.Name)
			done <- r
		}(r)
	}
	for range runs {
		<-done
	}
	for _, r := range runs {
		if r.err != nil {
			return fmt.Errorf("canary %s: %w", r.role, r.err)
		}
	}
	verdicts := map[string]map[string]string{}
	for _, r := range runs {
		verdicts[r.role] = r.out
	}
	return ClassifyCanaries(verdicts[CanaryDeny], verdicts[CanaryGateway])
}

// sweepCanaries deletes canaries a previous proof left behind (a policy
// service killed mid-proof); they hold quota and nothing else.
func (i *Inspector) sweepCanaries(ctx context.Context) {
	var list api.List[api.Pod]
	if err := i.o.Client.List(ctx, api.Pods, i.o.Namespace, api.ListOptions{LabelSelector: LabelCanary}, &list); err != nil {
		return
	}
	for _, pod := range list.Items {
		_ = i.o.Client.Delete(ctx, api.Pods, i.o.Namespace, pod.Metadata.Name, api.DeleteOptions{GracePeriodSeconds: api.Int64(0), UID: pod.Metadata.UID})
	}
}

// canarySpec is the pod of one canary: admitted by the namespace's
// admission policy (the RuntimeClass, the sandbox label, no token), no
// volumes, minimal resources, the egress label for the gateway role.
func (i *Inspector) canarySpec(role, suffix string, env []api.EnvVar) *api.Pod {
	name := "warden-canary-" + role + "-" + suffix
	labels := map[string]string{LabelSandbox: name, LabelCanary: role}
	if role == CanaryGateway {
		labels[LabelEgress] = EgressGateway
	}
	image := i.o.Canary.Image
	if image == "" {
		image = DefaultCanaryImage
	}
	return &api.Pod{
		Metadata: api.ObjectMeta{Name: name, Namespace: i.o.Namespace, Labels: labels},
		Spec: api.PodSpec{
			PriorityClassName:             i.o.Canary.PriorityClass,
			RuntimeClassName:              api.String(i.o.RuntimeClass),
			AutomountServiceAccountToken:  api.Bool(false),
			RestartPolicy:                 "Never",
			TerminationGracePeriodSeconds: api.Int64(1),
			EnableServiceLinks:            api.Bool(false),
			SecurityContext:               &api.PodSecurityContext{RunAsNonRoot: api.Bool(true), RunAsUser: api.Int64(65534), RunAsGroup: api.Int64(65534), SeccompProfile: &api.SeccompProfile{Type: "RuntimeDefault"}},
			Containers: []api.Container{{
				Name:    "canary",
				Image:   image,
				Command: []string{"/bin/sh", "-c", canaryScript},
				Env:     env,
				Resources: api.ResourceRequirements{
					Requests: api.ResourceList{"cpu": "50m", "memory": "32Mi"},
					Limits:   api.ResourceList{"cpu": "100m", "memory": "64Mi"},
				},
				SecurityContext: &api.SecurityContext{AllowPrivilegeEscalation: api.Bool(false), Capabilities: &api.Capabilities{Drop: []string{"ALL"}}},
			}},
		},
	}
}

// awaitCanary waits for the pod to end and returns its parsed verdicts.
func (i *Inspector) awaitCanary(ctx context.Context, name string) (map[string]string, error) {
	interval := i.o.Canary.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		var pod api.Pod
		if err := i.o.Client.Get(ctx, api.Pods, i.o.Namespace, name, &pod); err != nil {
			if ctx.Err() != nil {
				return nil, errors.New("did not finish in time")
			}
			return nil, err
		}
		switch pod.Status.Phase {
		case "Succeeded":
			return i.readVerdicts(ctx, name)
		case "Failed":
			reason := pod.Status.Reason
			for _, s := range pod.Status.ContainerStatuses {
				if s.State.Terminated != nil && s.State.Terminated.Reason != "" {
					reason = s.State.Terminated.Reason
				}
			}
			if reason == "" {
				reason = "failed"
			}
			return nil, errors.New("pod " + reason)
		}
		for _, s := range pod.Status.ContainerStatuses {
			if w := s.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" || w.Reason == "InvalidImageName" || w.Reason == "CreateContainerError") {
				return nil, errors.New("pod " + w.Reason)
			}
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("did not finish in time")
		case <-time.After(interval):
		}
	}
}

// readVerdicts reads the canary's log and parses it.
func (i *Inspector) readVerdicts(ctx context.Context, name string) (map[string]string, error) {
	body, err := i.o.Client.Logs(ctx, i.o.Namespace, name, "canary", false)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return ParseCanaryLog(io.LimitReader(body, 64*1024))
}

// ParseCanaryLog parses a canary's output: "warden-canary <probe> <open|
// closed|unknown>" per probe and a final "warden-canary done". Lines that
// are not verdicts are ignored; a missing "done" or probe is an error.
func ParseCanaryLog(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	done := false
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, canaryPrefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, canaryPrefix))
		switch {
		case len(fields) == 1 && fields[0] == "done":
			done = true
		case len(fields) == 2 && contains(canaryProbes, fields[0]):
			switch fields[1] {
			case "open", "closed", "unknown":
				if _, dup := out[fields[0]]; dup {
					return nil, errors.New("probe " + fields[0] + " reported twice")
				}
				out[fields[0]] = fields[1]
			default:
				return nil, errors.New("probe " + fields[0] + " reported " + fields[1])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !done {
		return nil, errors.New("did not report every probe")
	}
	for _, probe := range canaryProbes {
		if _, ok := out[probe]; !ok {
			return nil, errors.New("probe " + probe + " missing")
		}
	}
	return out, nil
}

// ClassifyCanaries turns the two canaries' verdicts into the cluster fact:
// the unlabelled canary must reach nothing; the labelled one must reach
// the gateway and nothing else. The error names what was reached, or the
// gateway that was not.
func ClassifyCanaries(deny, gateway map[string]string) error {
	for _, probe := range canaryProbes {
		switch deny[probe] {
		case "closed":
		case "open":
			return errors.New("NetworkPolicy is not enforced: an unlabelled pod reached the " + probe)
		default:
			return errors.New("the unlabelled canary could not probe the " + probe)
		}
	}
	for _, probe := range canaryProbes {
		v := gateway[probe]
		if probe == "gateway" {
			switch v {
			case "open":
			case "closed":
				return errors.New("the gateway-labelled pod could not reach the gateway")
			default:
				return errors.New("the labelled canary could not probe the gateway")
			}
			continue
		}
		switch v {
		case "closed":
		case "open":
			return errors.New("the gateway label admits more than the gateway: a labelled pod reached the " + probe)
		default:
			return errors.New("the labelled canary could not probe the " + probe)
		}
	}
	return nil
}

// apiServer is the address pods reach the API server at.
func (i *Inspector) apiServer() (host, port string, err error) {
	if i.o.Canary.APIServer != "" {
		return splitHostPort(i.o.Canary.APIServer, "")
	}
	if h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"); h != "" && p != "" {
		return h, p, nil
	}
	u, err := url.Parse(i.o.Client.Host())
	if err != nil {
		return "", "", err
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		port = "443"
	}
	if host == "" {
		return "", "", errors.New("the API server address is unknown")
	}
	return host, port, nil
}

func splitHostPort(value, fallback string) (string, string, error) {
	if value == "" {
		value = fallback
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", "", err
	}
	if _, err := strconv.Atoi(port); err != nil || host == "" {
		return "", "", errors.New("host:port expected")
	}
	return host, port, nil
}

func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%100000000, 10)
	}
	return hex.EncodeToString(b[:])
}

// gatewayTries is how often a canary retries the gateway: the labelled one
// (expected to succeed) allows for endpoint propagation after a policy
// restart; the unlabelled one (expected to fail) probes once.
func gatewayTries(role string) string {
	if role == CanaryGateway {
		return "8"
	}
	return "1"
}
