package installer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/containerimage"
)

const policyProbeLabel = "oberth.ci/network-policy-probe"
const policyProbeExpiry = "oberth.ci/probe-expires-at"

type NetworkPolicyProbeResult struct{ Status, Remedy, Evidence string }
type policyProbeTiming struct{ total, ready, traffic, cleanup, interval time.Duration }

func ProbeNetworkPolicy(ctx context.Context, cfg Config, deps Deps) NetworkPolicyProbeResult {
	return probeNetworkPolicy(ctx, cfg, deps, policyProbeTiming{120 * time.Second, 30 * time.Second, 35 * time.Second, 30 * time.Second, 500 * time.Millisecond})
}

func probeNetworkPolicy(ctx context.Context, cfg Config, deps Deps, timing policyProbeTiming) (result NetworkPolicyProbeResult) {
	result.Status = "unverified"
	phase := "preparation"
	remedy := "Allow namespaces create/get/list/delete, Pods create/get, pods/exec get/create and NetworkPolicies create/delete; ensure the pinned utility image can be pulled. Repeat plan after repairing the reported phase."
	started := time.Now()
	defer func() {
		if result.Status != "enforced" {
			result.Remedy = fmt.Sprintf("NetworkPolicy egress is unverified during %s after %s. %s %s", phase, time.Since(started).Round(time.Millisecond), remedy, result.Remedy)
		}
	}()
	if deps.KubeClient == nil || deps.RunCommand == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timing.total)
	defer cancel()
	phase = "leftover probe reconciliation"
	leftovers, err := reconcilePolicyProbes(ctx, deps, timing)
	if err != nil {
		result.Remedy = leftovers
		return
	}
	phase = "node coverage"
	nodes, err := policyProbeNodes(ctx, deps)
	if err != nil || len(nodes) == 0 {
		return
	}
	phase = "pipeline namespace labels"
	target := cfg.ArgoNamespace
	if target == "" {
		target = DefaultArgoNamespace
	}
	labels := map[string]string{}
	existing, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, target, metav1.GetOptions{})
	coverage := "pipeline namespace not yet present; namespace-specific exclusions and installed allow rules are not tested"
	if err == nil {
		labels = maps.Clone(existing.Labels)
		coverage = "pipeline namespace labels copied except kubernetes.io/metadata.name; name-specific exclusions and installed allow rules are not tested"
	} else if !apierrors.IsNotFound(err) {
		return
	}
	delete(labels, "kubernetes.io/metadata.name")
	tokenBytes := make([]byte, 8)
	if _, err = rand.Read(tokenBytes); err != nil {
		return
	}
	token := hex.EncodeToString(tokenBytes)
	namespace := "oberth-policy-probe-" + token
	labels[policyProbeLabel] = token
	if _, exists := labels["pod-security.kubernetes.io/enforce"]; !exists {
		labels["pod-security.kubernetes.io/enforce"] = "restricted"
	}
	result.Evidence = fmt.Sprintf("TCP default-deny egress only; scratch namespace %s; pipeline namespace %s; client/control nodes %s; server node %s; %s. Ingress enforcement is not tested.", namespace, target, strings.Join(nodes, ","), nodes[0], coverage) + " " + leftovers
	var createdUID types.UID
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), timing.cleanup)
		defer stop()
		if err := cleanupObservedPolicyProbe(cleanupCtx, deps, namespace, token, createdUID, false, timing.interval); err != nil {
			result.Status = "unverified"
			result.Remedy += " Cleanup is unverified; inspect ownership and remove only scratch namespace " + namespace + " in context " + deps.ContextName + ", then repeat plan."
		}
	}()
	phase = "scratch namespace creation"
	created, createErr := deps.KubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: labels, Annotations: map[string]string{policyProbeExpiry: time.Now().Add(timing.total + timing.cleanup + time.Minute).UTC().Format(time.RFC3339)}}}, metav1.CreateOptions{})
	if createErr != nil {
		result.Remedy = policyProbeFailure(createErr)
		return
	}
	createdUID = created.UID
	phase = "Pod creation and readiness"
	image := cfg.UtilityImage
	if image == "" {
		image = containerimage.Utility
	}
	names := []string{"server"}
	for i := range nodes {
		names = append(names, fmt.Sprintf("client-%d", i), fmt.Sprintf("control-%d", i))
	}
	for _, name := range names {
		role := "client"
		node := nodes[0]
		if name == "server" {
			role = "server"
		} else {
			for i, n := range nodes {
				if name == fmt.Sprintf("client-%d", i) || name == fmt.Sprintf("control-%d", i) {
					node = n
					break
				}
			}
		}
		pod := policyProbePod(namespace, token, role, image)
		pod.Name = name
		pod.Spec.NodeName = node
		if strings.HasPrefix(name, "control-") {
			pod.Labels[policyProbeLabel] = token + "-control"
		}
		if name == "server" {
			pod.Labels[policyProbeLabel] = token + "-server"
		}
		if _, err = deps.KubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			result.Remedy = policyProbeFailure(err)
			return
		}
	}
	readyCtx, stop := context.WithTimeout(ctx, timing.ready)
	serverIP := ""
	err = pollPolicyProbe(readyCtx, timing.interval, func(ctx context.Context) (bool, error) {
		ready, ip, err := policyProbeHealthy(ctx, deps, namespace, names)
		serverIP = ip
		return ready, err
	})
	stop()
	if err != nil || net.ParseIP(serverIP) == nil {
		result.Remedy = policyProbeFailure(err)
		return
	}
	url := "http://" + net.JoinHostPort(serverIP, "8080") + "/"
	phase = "baseline traffic"
	if err = waitPolicyTraffic(ctx, cfg, deps, timing, namespace, names, url, ""); err != nil {
		result.Remedy = policyProbeFailure(err)
		return
	}
	policies := deps.KubeClient.NetworkingV1().NetworkPolicies(namespace)
	// Randomize which source is selected first; each source must then pass as
	// the unselected peer and fail as the selected peer on the same server.
	order := []string{"client", "control"}
	if tokenBytes[0]&1 != 0 {
		slices.Reverse(order)
	}
	for round, selected := range order {
		selector := token
		if selected == "control" {
			selector += "-control"
		}
		result.Evidence += fmt.Sprintf(" Round %d selects %s Pods.", round+1, selected)
		phase = "egress policy programming"
		_, err = policies.Create(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: namespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{policyProbeLabel: selector}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}, metav1.CreateOptions{})
		if err != nil {
			result.Remedy = policyProbeFailure(err)
			return
		}
		phase = "egress convergence (a bounded delay does not prove absence)"
		if err = waitPolicyTraffic(ctx, cfg, deps, timing, namespace, names, url, selected); err != nil {
			result.Remedy = policyProbeFailure(err)
			return
		}
		result.Evidence += fmt.Sprintf(" %d paired denials verified: unselected HTTP response started before and completed after each selected attempt.", 3*len(nodes))
		phase = "egress policy removal"
		if err = policies.Delete(ctx, "default-deny", metav1.DeleteOptions{}); err != nil {
			result.Remedy = policyProbeFailure(err)
			return
		}
		phase = "traffic recovery"
		if err = waitPolicyTraffic(ctx, cfg, deps, timing, namespace, names, url, ""); err != nil {
			result.Remedy = policyProbeFailure(err)
			return
		}
	}
	result.Evidence += " Both sources connected at baseline and after each policy removal."
	result.Status = "enforced"
	return
}

func policyProbeNodes(ctx context.Context, deps Deps) ([]string, error) {
	list, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, node := range list.Items {
		ready := false
		for _, c := range node.Status.Conditions {
			ready = ready || c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
		}
		tainted := false
		for _, taint := range node.Spec.Taints {
			tainted = tainted || taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute
		}
		if ready && !node.Spec.Unschedulable && !tainted {
			nodes = append(nodes, node.Name)
		}
	}
	slices.Sort(nodes)
	return nodes, nil
}

func policyProbeHealthy(ctx context.Context, deps Deps, namespace string, names []string) (bool, string, error) {
	ip := ""
	for _, name := range names {
		pod, err := deps.KubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, "", err
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			ready = ready || c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		}
		if !ready || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed {
			return false, "", nil
		}
		if name == "server" {
			ip = pod.Status.PodIP
		}
	}
	return true, ip, nil
}

func waitPolicyTraffic(ctx context.Context, cfg Config, deps Deps, timing policyProbeTiming, namespace string, names []string, url string, selected string) error {
	stage, stop := context.WithTimeout(ctx, timing.traffic)
	defer stop()
	counts := map[string]int{}
	last := ""
	err := pollPolicyProbe(stage, timing.interval, func(ctx context.Context) (bool, error) {
		ready, _, err := policyProbeHealthy(ctx, deps, namespace, names)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, errors.New("probe Pod health changed")
		}
		all := true
		for _, name := range names {
			if !strings.HasPrefix(name, "client-") {
				continue
			}
			control := strings.Replace(name, "client-", "control-", 1)
			if selected == "control" {
				name, control = control, name
			}
			var state string
			if selected == "" {
				for _, peer := range []string{name, control} {
					state, err = policyProbeTraffic(ctx, cfg, deps, namespace, peer, url)
					if err != nil {
						return false, err
					}
					if state != "allowed" {
						last = "Client " + peer + ": " + policyTrafficDescription(state) + "; expected a successful connection."
						break
					}
				}
			} else {
				state, err = policyProbePair(ctx, cfg, deps, timing, namespace, name, control, url)
				if err != nil {
					return false, err
				}
			}
			matches := state == "allowed"
			want := "a successful connection"
			if selected != "" {
				matches = state == "timeout" || state == "refused" || state == "reset"
				want = "a timeout, refusal or reset while selected, with verified peer overlap"
			}
			if matches {
				counts[name]++
			} else {
				counts[name] = 0
				if selected != "" {
					last = fmt.Sprintf("Client %s: %s; expected %s.", name, policyTrafficDescription(state), want)
				}
			}
			all = all && counts[name] >= 3
		}
		return all, nil
	})
	if err != nil && last != "" {
		return &policyTrafficError{last + " " + policyProbeFailure(err), err}
	}
	return err
}

// A partial HTTP response is the start handshake, not a goroutine-start signal.
// Its end marker must still be absent after the selected attempt. Only then can
// the successful full response corroborate that attempt. Every worker is joined.
func policyProbePair(ctx context.Context, cfg Config, deps Deps, timing policyProbeTiming, namespace, client, control, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	nonce := rand.Text()
	finished := make(chan struct{})
	var controlState string
	var controlErr error
	var completed time.Time
	go func() {
		defer close(finished)
		controlState, controlErr = policyProbeExec(ctx, cfg, deps, namespace, control, policyOverlapScript, url, nonce)
		completed = time.Now()
	}()
	defer func() { cancel(); <-finished }()
	checkOpen := func(ctx context.Context) (bool, error) {
		state, err := policyProbeExec(ctx, cfg, deps, namespace, control, policyOverlapReadyScript, nonce)
		if err != nil {
			return false, err
		}
		switch state {
		case "open":
			return true, nil
		case "pending":
			return false, nil
		default:
			return false, errors.New("unselected HTTP response is already complete or invalid")
		}
	}
	// Timestamp the handshake operation, rather than two adjacent clock reads
	// after it (which can be equal on macOS). The remote markers establish
	// the actual HTTP span; this bounds its observation before the attempt.
	handshakeStarted := time.Now()
	start, stop := context.WithTimeout(ctx, 3*time.Second)
	err := pollPolicyProbe(start, timing.interval, func(ctx context.Context) (bool, error) {
		select {
		case <-finished:
			return false, errors.New("unselected request ended before the overlap handshake")
		default:
			return checkOpen(ctx)
		}
	})
	stop()
	if err != nil {
		return "", &policyTrafficError{"Unselected client " + control + ": HTTP overlap start could not be established. " + policyProbeFailure(err), err}
	}
	attemptStarted := time.Now()
	state, err := policyProbeTraffic(ctx, cfg, deps, namespace, client, url)
	attemptCompleted := time.Now()
	if err != nil {
		return "", err
	}
	open, err := checkOpen(ctx)
	if err != nil || !open {
		return "", &policyTrafficError{"Selected client " + client + ": " + policyTrafficDescription(state) + "; unselected HTTP response did not remain open through the attempt.", err}
	}
	released, err := policyProbeExec(ctx, cfg, deps, namespace, "server", policyOverlapReleaseScript, nonce)
	if err != nil || released != "released" {
		return "", &policyTrafficError{"Unselected HTTP response could not be released after the selected attempt.", err}
	}
	select {
	case <-finished:
	case <-ctx.Done():
		return "", &policyTrafficError{"Unselected HTTP response did not complete within the overlap budget.", ctx.Err()}
	}
	if controlErr != nil || controlState != "allowed" || !handshakeStarted.Before(attemptStarted) || !completed.After(attemptCompleted) {
		return "", &policyTrafficError{"Selected client " + client + ": " + policyTrafficDescription(state) + "; unselected client " + control + ": " + policyTrafficDescription(controlState) + "; HTTP overlap was not verified.", controlErr}
	}
	return state, nil
}

// The server flushes a sizeable first chunk, then holds the same HTTP response
// open until released after the selected attempt (at most ten seconds). Tokens
// tie the observed start/end to this request, never a previous one.
const policyOverlapServerScript = `mkdir -p /probe/cgi-bin
printf oberth-policy-probe > /probe/index.html
cat > /probe/cgi-bin/hold <<'CGI'
#!/bin/sh
printf 'Content-Type: text/plain\r\n\r\noberth-control-start:%s\n' "$QUERY_STRING"
dd if=/dev/zero bs=1024 count=16 2>/dev/null | tr '\000' x
for i in 1 2 3 4 5 6 7 8 9 10; do
 if [ -f "/probe/release-$QUERY_STRING" ]; then
  printf '\noberth-control-end:%s\n' "$QUERY_STRING"
  exit
 fi
 sleep 1
done
CGI
chmod 755 /probe/cgi-bin/hold
exec httpd -f -p 8080 -h /probe`

const policyOverlapScript = `body="/probe/overlap-$2"
error="$body-error"
if wget -S -T 10 -O "$body" "$1/cgi-bin/hold?$2" 2>"$error"; then
 if grep -q "^oberth-control-start:$2$" "$body" && grep -q "^oberth-control-end:$2$" "$body"; then printf allowed; else printf invalid; fi
else printf invalid; fi`

const policyOverlapReleaseScript = `touch "/probe/release-$1" && printf released`

const policyOverlapReadyScript = `body="/probe/overlap-$1"
if grep -q "^oberth-control-end:$1$" "$body" 2>/dev/null; then printf closed
elif grep -q "^oberth-control-start:$1$" "$body" 2>/dev/null; then printf open
else printf pending; fi`

// Transport failures are only denial candidates when the independent control
// path works and both baseline and post-policy recovery succeed. Keep outcomes
// distinct so an unverified result can describe what the operator observed.
const policyTrafficScript = `command -v wget >/dev/null || { printf invalid; exit; }
if wget -S -T 2 -O /probe/body "$1" 2>/probe/error; then
 if [ "$(cat /probe/body)" = oberth-policy-probe ]; then printf allowed; else printf invalid; fi
elif grep -E 'HTTP/[0-9]|server returned error' /probe/error >/dev/null; then printf invalid
elif grep -i 'connection refused' /probe/error >/dev/null; then printf refused
elif grep -Ei 'connection.*reset|reset by peer' /probe/error >/dev/null; then printf reset
elif grep -i 'timed out' /probe/error >/dev/null; then printf timeout
else printf invalid; fi`

func policyProbeTraffic(ctx context.Context, cfg Config, deps Deps, namespace, pod, url string) (string, error) {
	attempt, stop := context.WithTimeout(ctx, 4*time.Second)
	defer stop()
	state, err := policyProbeExec(attempt, cfg, deps, namespace, pod, policyTrafficScript, url)
	if err != nil {
		return "", &policyTrafficError{"Traffic observation unavailable: kubectl exec did not complete.", err}
	}
	switch state {
	case "allowed", "timeout", "refused", "reset":
	default:
		return "", &policyTrafficError{"Traffic observation ambiguous: missing wget, HTTP error or unexpected response.", nil}
	}
	return state, nil
}

func policyProbeExec(ctx context.Context, cfg Config, deps Deps, namespace, pod, script string, values ...string) (string, error) {
	args := []string{"--context", deps.ContextName, "--namespace", namespace, "exec", pod, "--", "sh", "-c", script, "sh"}
	args = append(args, values...)
	if cfg.KubeconfigPath != "" {
		args = append([]string{"--kubeconfig", cfg.KubeconfigPath}, args...)
	}
	raw, err := deps.RunCommand(ctx, nil, "kubectl", args...)
	return strings.TrimSpace(string(raw)), err
}

func pollPolicyProbe(ctx context.Context, interval time.Duration, check func(context.Context) (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := check(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func policyProbePod(namespace, token, role, image string) *corev1.Pod {
	command := []string{"sleep", "180"}
	if role == "server" {
		command = []string{"sh", "-ec", policyOverlapServerScript}
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: namespace, Labels: map[string]string{policyProbeLabel: token}}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever, ActiveDeadlineSeconds: ptr.To[int64](180), TerminationGracePeriodSeconds: ptr.To[int64](0),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65534), RunAsGroup: ptr.To[int64](65534), FSGroup: ptr.To[int64](65534), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Volumes:         []corev1.Volume{{Name: "probe", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("8Mi"))}}}},
		Containers:      []corev1.Container{{Name: "probe", Image: image, Command: command, VolumeMounts: []corev1.VolumeMount{{Name: "probe", MountPath: "/probe"}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")}}}},
	}}
}

type policyTrafficError struct {
	detail string
	cause  error
}

func (e *policyTrafficError) Error() string { return e.detail }
func (e *policyTrafficError) Unwrap() error { return e.cause }

func policyTrafficDescription(state string) string {
	switch state {
	case "allowed":
		return "connection succeeded"
	case "timeout":
		return "connection timed out"
	case "refused":
		return "connection refused"
	case "reset":
		return "connection reset"
	default:
		return "traffic observation ambiguous"
	}
}

func policyProbeFailure(err error) string {
	var traffic *policyTrafficError
	if errors.As(err, &traffic) {
		return traffic.detail
	}
	if err == nil {
		return ""
	}
	switch {
	case apierrors.IsForbidden(err):
		return "The API denied the required permission."
	case apierrors.IsUnauthorized(err):
		return "The selected API credentials were not accepted."
	case errors.Is(err, context.DeadlineExceeded):
		return "The observation/convergence budget expired; the observed delay is not proof of absence."
	case errors.Is(err, context.Canceled):
		return "The observation was cancelled."
	default:
		return "The API, Pod health or traffic observation could not be verified."
	}
}
