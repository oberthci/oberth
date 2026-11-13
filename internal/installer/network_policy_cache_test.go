package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

type policyPlanFixture struct {
	cfg   Config
	deps  Deps
	host  InstallDeps
	kube  *fake.Clientset
	calls int
	root  string
}

func newPolicyPlanFixture(t *testing.T) *policyPlanFixture {
	t.Helper()
	node := preflightNode()
	node.UID = "node-uid"
	kube := fake.NewClientset(node, preflightStorage(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultArgoNamespace, UID: "pipeline-uid", Labels: map[string]string{"team": "build"}}})
	f := &policyPlanFixture{cfg: Config{ContextName: "chosen", Namespace: DefaultNamespace, NetworkPolicy: "true", ChartPath: testLocalChart}, kube: kube, root: t.TempDir()}
	config := &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA(t, []string{"api.example.test"})}}
	probe := func(context.Context, Config, Deps) NetworkPolicyProbeResult {
		f.calls++
		return NetworkPolicyProbeResult{Status: "enforced", Evidence: "complete live measurement and owned cleanup"}
	}
	cacheDir := func() (string, error) { return f.root, nil }
	helm := func(ctx context.Context, _ []string) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []byte("[]"), nil
	}
	f.deps = Deps{KubeClient: kube, RestConfig: config, ContextName: "chosen", RunCommand: preflightStats, RunHelm: helm, ProbeNetworkPolicy: probe, policyProbeCacheDir: cacheDir}
	f.host = InstallDeps{LookPath: func(name string) (string, error) { return name, nil }, RunHelm: helm, RunCommand: preflightStats, ProbeNetworkPolicy: probe, policyProbeCacheDir: cacheDir,
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) { return kube, config, "chosen", nil }}
	return f
}
func (f *policyPlanFixture) plan(t *testing.T) {
	t.Helper()
	plan := PlanInstall(t.Context(), f.cfg, f.host)
	if !plan.OK {
		t.Fatalf("plan failed: %+v", plan.Checks)
	}
}
func (f *policyPlanFixture) preflight(t *testing.T) InstallResult {
	t.Helper()
	cfg := f.cfg
	result := InstallResult{OK: true}
	inspectInstallPreflight(t.Context(), &cfg, f.deps, f.host, "chosen", &ClusterIdentity{}, &result)
	return result
}
func (f *policyPlanFixture) receipt(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.root, "oberth", "network-policy-plans", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("receipt files %v: %v", paths, err)
	}
	return paths[0]
}

func TestPlanMeasurementReusedOnceByInstallPreflight(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.plan(t)
	if f.calls != 1 {
		t.Fatal("plan omitted measurement", f.calls)
	}
	result := f.preflight(t)
	if !result.OK || f.calls != 1 {
		t.Fatal("install repeated plan measurement", result.Checks, f.calls)
	}
	var reused bool
	for _, c := range result.Checks {
		if c.Code == "network_policy" {
			reused = strings.Contains(c.Evidence, "Reused successful plan")
		}
	}
	if !reused {
		t.Fatal("receipt age/provenance absent", result.Checks)
	}
	f.preflight(t)
	if f.calls != 2 {
		t.Fatal("receipt reused twice", f.calls)
	}
	// An install result cannot seed reuse for another fresh install.
	f.preflight(t)
	if f.calls != 3 {
		t.Fatal("install seeded a new receipt", f.calls)
	}
}

func TestPlanMeasurementReusedByExecute(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.plan(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var reached bool
	f.host.installResolved = func(Config) { reached = true; cancel() }
	f.host.CredentialSink = func(string, string) {}
	err := Execute(ctx, f.cfg, f.host)
	if !reached || !errors.Is(err, context.Canceled) || f.calls != 1 {
		t.Fatal("Execute did not reuse plan measurement", reached, f.calls, err)
	}
}

func TestPolicyPlanMismatchAndExpiryRequireFreshMeasurement(t *testing.T) {
	for _, change := range []string{"expired", "future", "malformed", "wrong binding", "failed", "permissions", "cluster UID", "CA", "endpoint", "context", "namespace", "namespace UID", "namespace labels", "namespace absent", "node UID", "node labels", "node taint", "image", "install namespace"} {
		t.Run(change, func(t *testing.T) {
			f := newPolicyPlanFixture(t)
			f.plan(t)
			path := f.receipt(t)
			switch change {
			case "expired", "future", "wrong binding", "failed":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var r policyPlanMeasurement
				if err = json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "expired":
					r.CompletedAt = time.Now().Add(-policyPlanValidity)
				case "future":
					r.CompletedAt = time.Now().Add(time.Minute)
				case "wrong binding":
					r.Binding = "other"
				case "failed":
					r.Result.Status = "unverified"
				}
				raw, _ = json.Marshal(r)
				if err = os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "cluster UID":
				ns, _ := f.kube.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
				ns.UID = "other"
				_, _ = f.kube.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
			case "CA":
				f.deps.RestConfig.CAData = newTestServerCA(t, []string{"other.example.test"})
			case "endpoint":
				f.deps.RestConfig.Host = "https://other.example.test"
			case "context":
				f.deps.ContextName = "other"
			case "namespace":
				f.cfg.ArgoNamespace = "other-pipelines"
			case "namespace UID", "namespace labels":
				ns, _ := f.kube.CoreV1().Namespaces().Get(t.Context(), DefaultArgoNamespace, metav1.GetOptions{})
				if change == "namespace UID" {
					ns.UID = "replacement"
				} else {
					ns.Labels["team"] = "changed"
				}
				_, _ = f.kube.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
			case "namespace absent":
				_ = f.kube.CoreV1().Namespaces().Delete(t.Context(), DefaultArgoNamespace, metav1.DeleteOptions{})
			case "node UID", "node labels", "node taint":
				node, _ := f.kube.CoreV1().Nodes().Get(t.Context(), "node", metav1.GetOptions{})
				switch change {
				case "node UID":
					node.UID = "replacement"
				case "node labels":
					node.Labels = map[string]string{"pool": "other"}
				case "node taint":
					node.Spec.Taints = []corev1.Taint{{Key: "changed", Effect: corev1.TaintEffectNoSchedule}}
				}
				_, _ = f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			case "image":
				f.cfg.UtilityImage = "different-image"
			case "install namespace":
				f.cfg.Namespace = "other-oberth"
			}
			result := f.preflight(t)
			if !result.OK || f.calls != 2 {
				t.Fatal("mismatched receipt bypassed live probe", f.calls, result.Checks)
			}
		})
	}
}

func TestFreshInstallGateAndOtherChecksRemainMandatory(t *testing.T) {
	for _, recentPlan := range []bool{false, true} {
		f := newPolicyPlanFixture(t)
		if recentPlan {
			f.plan(t)
		}
		f.deps.ProbeNetworkPolicy = func(context.Context, Config, Deps) NetworkPolicyProbeResult {
			f.calls++
			return NetworkPolicyProbeResult{Status: "unverified", Remedy: "probe failed"}
		}
		if recentPlan {
			// A valid network proof cannot replace a current disk check.
			f.deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
				return nil, errors.New("stats unavailable")
			}
		}
		result := f.preflight(t)
		if result.OK {
			t.Fatal("fresh-install gate weakened", recentPlan, result.Checks)
		}
		if !recentPlan && f.calls != 1 {
			t.Fatal("install without proof omitted live measurement", f.calls)
		}
	}
}

func TestUnverifiedPlanDoesNotSeedReuse(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.host.ProbeNetworkPolicy = func(context.Context, Config, Deps) NetworkPolicyProbeResult {
		f.calls++
		return NetworkPolicyProbeResult{Status: "unverified", Evidence: "incomplete", Remedy: "fix enforcement"}
	}
	plan := PlanInstall(t.Context(), f.cfg, f.host)
	if plan.OK {
		t.Fatal("unverified plan accepted")
	}
	f.preflight(t)
	if f.calls != 2 {
		t.Fatal("failed plan seeded cache", f.calls)
	}
}

func TestPolicyPlanReceiptHasSingleConsumer(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.plan(t)
	path := f.receipt(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt policyPlanMeasurement
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Go(func() { _, ok := consumePolicyPlan(filepath.Dir(path), receipt.Binding, time.Now()); results <- ok })
	}
	wg.Wait()
	close(results)
	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("receipt consumers", successes)
	}
	if bytes.Contains(raw, []byte("token")) {
		t.Fatal("receipt contains credential material")
	}
}

func TestFailedNewPlanRevokesEarlierMeasurement(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.plan(t)
	f.host.ProbeNetworkPolicy = func(context.Context, Config, Deps) NetworkPolicyProbeResult {
		f.calls++
		return NetworkPolicyProbeResult{Status: "unverified", Evidence: "later measurement failed"}
	}
	if PlanInstall(t.Context(), f.cfg, f.host).OK {
		t.Fatal("failed plan accepted")
	}
	f.preflight(t)
	if f.calls != 3 {
		t.Fatal("older successful plan survived failed measurement", f.calls)
	}
}

func TestPolicyPlanNamespaceCreationInvalidatesAbsence(t *testing.T) {
	f := newPolicyPlanFixture(t)
	if err := f.kube.CoreV1().Namespaces().Delete(t.Context(), DefaultArgoNamespace, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.plan(t)
	if _, err := f.kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultArgoNamespace, UID: "new-pipeline"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.preflight(t)
	if f.calls != 2 {
		t.Fatal("namespace creation reused absent-namespace proof", f.calls)
	}
}

// The hardened measurement selects its container and fences Pod/policy UIDs.
// A receipt from the earlier measurement cannot satisfy this install's gate.
func TestPolicyPlanRejectsEarlierMeasurementReceipt(t *testing.T) {
	f := newPolicyPlanFixture(t)
	f.plan(t)
	path := f.receipt(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt policyPlanMeasurement
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Version = 1
	raw, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f.deps.ProbeNetworkPolicy = func(context.Context, Config, Deps) NetworkPolicyProbeResult {
		f.calls++
		return NetworkPolicyProbeResult{Status: "unverified", Remedy: "fresh hardened measurement required"}
	}
	result := f.preflight(t)
	if result.OK || f.calls != 2 {
		t.Fatal("earlier measurement receipt bypassed fresh enforcement gate", result.Checks, f.calls)
	}
}
