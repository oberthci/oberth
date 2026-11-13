package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const policyPlanValidity = 5 * time.Minute

// Receipt versions bind the measurement protocol as well as its schema. Version2
// selects the probe container and checks created Pod and NetworkPolicy UIDs.
const policyPlanVersion = 2

type policyPlanMeasurement struct {
	Version     int                      `json:"version"`
	Binding     string                   `json:"binding"`
	CompletedAt time.Time                `json:"completed_at"`
	Result      NetworkPolicyProbeResult `json:"result"`
}

// Only a successful plan can supply this single-use receipt. Every other
// preflight check still runs; absent, stale or unreadable proof runs the full
// live measurement. Neither install results nor public plan JSON grant reuse.
func networkPolicyForInstall(ctx context.Context, cfg Config, deps Deps, nodes []corev1.Node) NetworkPolicyProbeResult {
	binding := policyPlanBinding(ctx, cfg, deps, nodes)
	root := ""
	if binding != "" {
		root = policyPlanCacheRoot(deps)
	}
	if cfg.policyProbePlan && binding != "" && root != "" {
		// A newer failed plan must revoke earlier successful proof.
		_ = os.Remove(filepath.Join(root, binding+".json"))
	}
	if !cfg.policyProbePlan && binding != "" && root != "" && ctx.Err() == nil {
		if cached, ok := consumePolicyPlan(root, binding, time.Now()); ok {
			return cached
		}
	}
	probe := deps.ProbeNetworkPolicy
	if probe == nil {
		probe = ProbeNetworkPolicy
	}
	result := probe(ctx, cfg, deps)
	if cfg.policyProbePlan && result.Status == "enforced" && result.Evidence != "" && ctx.Err() == nil && binding != "" && root != "" {
		current, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil || binding != policyPlanBinding(ctx, cfg, deps, current.Items) {
			return result
		}
		savePolicyPlan(root, policyPlanMeasurement{Version: policyPlanVersion, Binding: binding, CompletedAt: time.Now().UTC(), Result: result})
	}
	return result
}

func policyPlanBinding(ctx context.Context, cfg Config, deps Deps, nodes []corev1.Node) string {
	identity := publicClusterIdentity(cfg, deps.RestConfig, deps.ContextName)
	if deps.KubeClient == nil || deps.RestConfig == nil || deps.RestConfig.Insecure || identity.CAPin == "" || !strings.HasPrefix(identity.Server, "https://") || len(nodes) == 0 {
		return ""
	}
	cluster, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || cluster.UID == "" {
		return ""
	}
	namespace := cfg.ArgoNamespace
	if namespace == "" {
		namespace = DefaultArgoNamespace
	}
	target, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return ""
	}
	if err == nil && (target.UID == "" || target.DeletionTimestamp != nil) {
		return ""
	}
	type nodeIdentity struct {
		Name, UID     string
		Unschedulable bool
		Taints        []corev1.Taint
		Labels        map[string]string
	}
	selected := make([]nodeIdentity, 0, len(nodes))
	for _, node := range nodes {
		if node.UID == "" {
			return ""
		}
		selected = append(selected, nodeIdentity{Name: node.Name, UID: string(node.UID), Unschedulable: node.Spec.Unschedulable, Taints: node.Spec.Taints, Labels: node.Labels})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	// Map encoding is deterministic; namespace absence is distinct from a
	// recreated namespace, and label/node changes invalidate the measurement.
	oberthNamespace := cfg.Namespace
	if oberthNamespace == "" {
		oberthNamespace = DefaultNamespace
	}
	object := struct {
		Version, Rounds, Samples                                                      int
		Server, CA, ClusterUID, Context, Namespace, TargetUID, OberthNamespace, Image string
		Labels                                                                        map[string]string
		Nodes                                                                         []nodeIdentity
	}{Version: policyPlanVersion, Rounds: policyProbeRounds, Samples: policyProbeSamples, Server: identity.Server, CA: identity.CAPin, ClusterUID: string(cluster.UID), Context: deps.ContextName, Namespace: namespace, OberthNamespace: oberthNamespace, Image: cfg.UtilityImage, Nodes: selected}
	if target != nil && err == nil {
		object.TargetUID, object.Labels = string(target.UID), target.Labels
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func policyPlanCacheRoot(deps Deps) string {
	dir := deps.policyProbeCacheDir
	if dir == nil {
		dir = os.UserCacheDir
	}
	root, err := dir()
	if err != nil || root == "" {
		return ""
	}
	root = filepath.Join(root, "oberth", "network-policy-plans")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ""
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return ""
	}
	return root
}

func savePolicyPlan(root string, receipt policyPlanMeasurement) {
	file, err := os.CreateTemp(root, ".plan-*")
	if err != nil {
		return
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	err = json.NewEncoder(file).Encode(receipt)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return
	}
	_ = os.Rename(name, filepath.Join(root, receipt.Binding+".json"))
}

func consumePolicyPlan(root, binding string, now time.Time) (NetworkPolicyProbeResult, bool) {
	// Rename first so separate install processes cannot consume the same receipt.
	dir, err := os.MkdirTemp(root, ".consume-*")
	if err != nil {
		return NetworkPolicyProbeResult{}, false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "plan.json")
	if err := os.Rename(filepath.Join(root, binding+".json"), path); err != nil {
		return NetworkPolicyProbeResult{}, false
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: private receipt moved into our private directory; validate descriptor below.
	if err != nil {
		return NetworkPolicyProbeResult{}, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return NetworkPolicyProbeResult{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || info.Size() > 1<<20 {
		return NetworkPolicyProbeResult{}, false
	}
	var receipt policyPlanMeasurement
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || receipt.Version != policyPlanVersion || receipt.Binding != binding || receipt.Result.Status != "enforced" || receipt.Result.Evidence == "" || receipt.CompletedAt.IsZero() {
		return NetworkPolicyProbeResult{}, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return NetworkPolicyProbeResult{}, false
	}
	age := now.Sub(receipt.CompletedAt)
	if age < 0 || age >= policyPlanValidity {
		return NetworkPolicyProbeResult{}, false
	}
	result := receipt.Result
	result.Evidence += fmt.Sprintf(" Reused successful plan measurement from %s (age %s; valid for %s; single use).", receipt.CompletedAt.Format(time.RFC3339), age.Round(time.Second), policyPlanValidity)
	return result, true
}
