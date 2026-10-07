package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func watchFixture(t *testing.T) (Config, Deps, *fake.Clientset, watchAdoptionPlan, *bytes.Buffer) {
	t.Helper()
	off := false
	cert := string(secretWithCertificate(t, []string{"oberth"}, nil).Data["tls.crt"])
	objects := []runtime.Object{
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "cloudflared-watch", Namespace: "oberth", UID: types.UID("sa-uid"), ResourceVersion: "1"}, AutomountServiceAccountToken: &off},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "cloudflared-watch-openbao-ca", Namespace: "oberth", UID: types.UID("bao-uid"), ResourceVersion: "2"}, Data: map[string]string{"ca.crt": cert}},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "cloudflared-watch-oberth-origin-ca", Namespace: "oberth", UID: types.UID("origin-uid"), ResourceVersion: "3"}, Data: map[string]string{"ca.crt": cert}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "cloudflared-watch-oberth-v2", Namespace: "oberth", UID: types.UID("deploy-uid"), ResourceVersion: "4"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "cloudflared-watch", Containers: []corev1.Container{{Name: "cloudflared", Image: "public-image"}}, InitContainers: []corev1.Container{{Name: "fetch-token", Image: "public-bao-image"}}}}}},
	}
	p := watchAdoptionPlan{Schema: watchAdoptionSchema, Namespace: "oberth", ChartVersion: "v0.16.25", ReleaseRevision: 71, ExpiresAt: time.Now().Add(time.Hour)}
	var rendered bytes.Buffer
	for _, o := range objects {
		v, e := watchSpec(o)
		if e != nil {
			t.Fatal(e)
		}
		kind := o.GetObjectKind().GroupVersionKind().Kind
		p.Objects = append(p.Objects, watchAdoptionObject{Kind: kind, Name: v.meta.GetName(), UID: string(v.meta.GetUID()), ResourceVersion: v.meta.GetResourceVersion(), Spec: v.spec, TargetSpec: v.spec})
		b, _ := json.Marshal(o)
		rendered.Write(b)
		rendered.WriteString("\n---\n")
	}
	cfg := Config{Namespace: "oberth", ChartVersion: p.ChartVersion, Upgrade: true, SkipArgo: true, WatchAdoptionPlan: filepath.Join(t.TempDir(), "public-plan.json")}
	writeWatchPlan(t, cfg, p)
	client := fake.NewClientset(objects...)
	var output bytes.Buffer
	deps := Deps{KubeClient: client, Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(`[{"name":"oberth","namespace":"oberth","revision":"71","status":"deployed","chart":"oberth-0.16.24"}]`), nil
		}
		joined := strings.Join(args, " ")
		// SSA preview from previewHelmUpgrade: --dry-run=server without --take-ownership.
		// This is a read-only conflict check; return success (no conflicts).
		if args[0] == "upgrade" && strings.Contains(joined, "--dry-run=server") && !strings.Contains(joined, "--take-ownership") {
			return json.Marshal(map[string]any{"name": "oberth", "namespace": "oberth", "version": 72, "manifest": "", "chart": map[string]any{"metadata": map[string]any{"name": "oberth", "version": "0.16.25"}}})
		}
		// Adoption preview: --dry-run=server with --take-ownership.
		if args[0] != "upgrade" || !strings.Contains(joined, "--dry-run=server") || !strings.Contains(joined, "--no-hooks") || !strings.Contains(joined, "--take-ownership") || !strings.Contains(joined, "--reuse-values") {
			t.Fatalf("unexpected mutation or helm call: %v", args)
		}
		return json.Marshal(map[string]any{"name": "oberth", "namespace": "oberth", "version": 72, "manifest": rendered.String(), "chart": map[string]any{"metadata": map[string]any{"name": "oberth", "version": "0.16.25"}}})
	}}
	// Model API JSONPatch test+atomic metadata update and monotonically changed RV.
	// Also accept ApplyPatchType for the data field ownership transfer (#813).
	client.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		x := a.(ktesting.PatchAction)
		// Allow SSA apply patches for data field ownership transfer.
		if x.GetPatchType() == types.ApplyPatchType {
			return false, nil, nil // let the default handler process it
		}
		if x.GetPatchType() != types.JSONPatchType {
			t.Fatal("non-CAS patch")
		}
		old, e := client.Tracker().Get(a.GetResource(), a.GetNamespace(), x.GetName())
		if e != nil {
			return true, nil, e
		}
		raw, _ := json.Marshal(old)
		patch, e := jsonpatch.DecodePatch(x.GetPatch())
		if e != nil {
			return true, nil, e
		}
		b, e := patch.Apply(raw)
		if e != nil {
			return true, nil, e
		}
		next := old.DeepCopyObject()
		if e = json.Unmarshal(b, next); e != nil {
			return true, nil, e
		}
		m, e := meta.Accessor(next)
		if e != nil {
			t.Fatal(e)
		}
		m.SetResourceVersion("1" + m.GetResourceVersion())
		if e = client.Tracker().Update(a.GetResource(), next, a.GetNamespace()); e != nil {
			return true, nil, e
		}
		return true, next, nil
	})
	return cfg, deps, client, p, &output
}

func TestWatchPlanRejectsCaseAliasesAndNull(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(p map[string]any) { p["Namespace"] = p["namespace"]; delete(p, "namespace") },
		func(p map[string]any) { p["expires_at"] = nil },
		func(p map[string]any) { p["objects"].([]any)[0].(map[string]any)["Kind"] = "ServiceAccount" },
		func(p map[string]any) { p["objects"].([]any)[0].(map[string]any)["spec"] = nil },
		func(p map[string]any) {
			p["objects"].([]any)[0].(map[string]any)["ownership"].(map[string]any)["Namespace"] = ""
		},
	} {
		cfg, deps, c, p, _ := watchFixture(t)
		raw, _ := json.Marshal(p)
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		mutate(fields)
		raw, _ = json.Marshal(fields)
		if err := os.WriteFile(cfg.WatchAdoptionPlan, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := adoptWatchTunnel(context.Background(), cfg, deps, false); err == nil || watchPatchCount(c) != 0 {
			t.Fatal("ambiguous plan reached metadata effects")
		}
	}
	if sameWatchJSON([]byte(`{"n":9007199254740992}`), []byte(`{"n":9007199254740993}`)) {
		t.Fatal("distinct reviewed integers rounded equal")
	}
}

func TestRunWatchAdoptionRetainsPublicReceiptOnFollowingFailure(t *testing.T) {
	for _, boundary := range []string{"helm", "revision", "expiry"} {
		t.Run(boundary, func(t *testing.T) {
			cfg, deps, c, p, out := watchFixture(t)
			cfg.Timeout = time.Second
			if boundary == "expiry" {
				p.ExpiresAt = time.Now().Add(2 * time.Second)
				writeWatchPlan(t, cfg, p)
			}
			if err := c.Tracker().Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3s-node"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.3+k3s1"}}}); err != nil {
				t.Fatal(err)
			}
			if err := c.Tracker().Add(readyArgoControllerPod()); err != nil {
				t.Fatal(err)
			}
			deps.RestConfig = &rest.Config{Host: "https://127.0.0.1:6443"}
			deps.ContextName = "test-ctx"
			deps.PollInterval = time.Millisecond
			render := deps.RunHelm
			mutations := 0
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "repo" {
					return nil, nil
				}
				if args[0] == "show" {
					return []byte("image:\n  ref: " + canonicalGARPrefix + "oberth@sha256:" + strings.Repeat("a", 64) + "\n"), nil
				}
				if args[0] == "get" {
					return []byte(`{}`), nil
				}
				if args[0] == "list" && watchPatchCount(c) > 0 && boundary == "revision" {
					return []byte(`[{"name":"oberth","namespace":"oberth","revision":"72","status":"deployed","chart":"oberth-0.16.24"}]`), nil
				}
				if args[0] == "upgrade" && !strings.Contains(strings.Join(args, " "), "--dry-run=server") {
					if strings.Contains(strings.Join(args, " "), "--take-ownership") {
						t.Fatal("actual mutation got preview-only flag")
					}
					mutations++
					return nil, errors.New("simulated Helm failure")
				}
				return render(ctx, args)
			}
			if boundary == "expiry" {
				c.PrependReactor("patch", "*", func(ktesting.Action) (bool, runtime.Object, error) {
					time.Sleep(time.Until(p.ExpiresAt) + time.Millisecond)
					return false, nil, nil
				})
			}
			err := Run(context.Background(), cfg, deps)
			if err == nil || !strings.Contains(out.String(), `"uid":"sa-uid"`) {
				t.Fatalf("real Run lost quiet adoption receipt: %v output=%s", err, out.String())
			}
			want := 1
			if boundary == "helm" {
				// 4 metadata CAS patches + 2 ConfigMap data field ownership
				// transfers (#813) = 6 patches before the Helm upgrade.
				want = 6
			}
			if watchPatchCount(c) != want || (boundary != "helm" && mutations != 0) {
				t.Fatalf("continued after %s: patches=%d helm=%d", boundary, watchPatchCount(c), mutations)
			}
		})
	}
}
func writeWatchPlan(t *testing.T, cfg Config, p watchAdoptionPlan) {
	t.Helper()
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cfg.WatchAdoptionPlan, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func watchPatchCount(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}
func TestWatchAdoptionPreflightsAllAndChangesOnlyMetadata(t *testing.T) {
	cfg, deps, c, p, out := watchFixture(t)
	if e := adoptWatchTunnel(context.Background(), cfg, deps, true); e != nil {
		t.Fatal(e)
	}
	if watchPatchCount(c) != 0 {
		t.Fatal("dry-run changed ownership")
	}
	c.ClearActions()
	if e := adoptWatchTunnel(context.Background(), cfg, deps, false); e != nil {
		t.Fatal(e)
	}
	// 4 metadata CAS patches + 2 ConfigMap data field ownership transfers (#813).
	if watchPatchCount(c) != 6 {
		t.Fatalf("wrong mutation inventory: got %d patches, want 6 (4 metadata + 2 data)", watchPatchCount(c))
	}
	for i, a := range c.Actions() {
		if i < 4 && a.GetVerb() != "get" {
			t.Fatal("mutation before full inventory read")
		}
	}
	for _, o := range p.Objects {
		v, e := getWatchObject(context.Background(), deps, p.Namespace, o)
		if e != nil || !sameWatchJSON(v.spec, o.Spec) || watchOwner(v.meta) != (watchOwnership{"Helm", "oberth", "oberth"}) {
			t.Fatal("adoption modified security spec or lost ownership")
		}
	}
	if !strings.Contains(out.String(), "deploy-uid") {
		t.Fatal("missing bounded public completion receipt")
	}
}
func TestWatchAdoptionRefusesEveryPreflightDriftWithoutMutation(t *testing.T) {
	for _, change := range []string{"namespace", "foreign-owner", "partial-owner", "uid", "rv", "spec", "target", "scope", "expiry", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			cfg, deps, c, p, _ := watchFixture(t)
			switch change {
			case "namespace":
				p.Namespace = "other"
			case "foreign-owner":
				p.Objects[3].Ownership = watchOwnership{"Helm", "foreign", "oberth"}
			case "partial-owner":
				p.Objects[3].Ownership = watchOwnership{ManagedBy: "Helm"}
			case "uid":
				p.Objects[3].UID = "replacement"
			case "rv":
				p.Objects[3].ResourceVersion = "999"
			case "spec":
				p.Objects[3].Spec = json.RawMessage(`{}`)
			case "target":
				p.Objects[3].TargetSpec = json.RawMessage(`{}`)
			case "scope":
				p.Objects[3].Name = "oberth"
			case "expiry":
				p.ExpiresAt = time.Now().Add(-time.Minute)
			case "duplicate":
				p.Objects[3] = p.Objects[0]
			}
			writeWatchPlan(t, cfg, p)
			if e := adoptWatchTunnel(context.Background(), cfg, deps, false); e == nil {
				t.Fatal("drift accepted")
			}
			if watchPatchCount(c) != 0 {
				t.Fatal("preflight rejection mutated earlier object")
			}
		})
	}
}
func TestWatchAdoptionRetainsPartialCASAndReportsExactSubset(t *testing.T) {
	cfg, deps, c, p, _ := watchFixture(t)
	calls := 0
	c.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 2 {
			return true, nil, errors.New("ambiguous response")
		}
		return false, nil, nil
	})
	e := adoptWatchTunnel(context.Background(), cfg, deps, false)
	if e == nil || !strings.Contains(e.Error(), `"uid":"sa-uid"`) || strings.Contains(e.Error(), `"uid":"deploy-uid"`) {
		t.Fatalf("partial receipt wrong: %v", e)
	}
	if calls != 2 {
		t.Fatal("retried or continued after unknown outcome")
	}
	v, e := getWatchObject(context.Background(), deps, p.Namespace, p.Objects[0])
	if e != nil || watchOwner(v.meta).Release != "oberth" {
		t.Fatal("rolled back confirmed adoption")
	}
}
func TestWatchAdoptionCASRejectsReplacementBetweenPreflightAndPatch(t *testing.T) {
	cfg, deps, c, _, _ := watchFixture(t)
	c.PrependReactor("patch", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
		g := schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
		o, e := c.Tracker().Get(g, "oberth", "cloudflared-watch")
		if e != nil {
			t.Fatal(e)
		}
		sa := o.(*corev1.ServiceAccount)
		sa.UID = "replaced-after-preflight"
		if e = c.Tracker().Update(g, sa, "oberth"); e != nil {
			t.Fatal(e)
		}
		return false, nil, nil
	})
	if e := adoptWatchTunnel(context.Background(), cfg, deps, false); e == nil {
		t.Fatal("replacement adopted")
	}
	if watchPatchCount(c) != 1 {
		t.Fatal("continued after CAS failure")
	}
}
func TestWatchAdoptionRejectsDuplicateJSONAndMixedCABundle(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{"a":1} {}`, `{"a":[{"b":1,"b":2}]}`} {
		if uniqueWatchJSON([]byte(raw)) {
			t.Fatalf("ambiguous JSON accepted: %s", raw)
		}
	}
	cert := string(secretWithCertificate(t, nil, nil).Data["tls.crt"])
	if !watchPublicCA(map[string]string{"ca.crt": cert}) {
		t.Fatal("public self-signed origin certificate rejected")
	}
	if watchPublicCA(map[string]string{"ca.crt": cert + "\n-----BEGIN PRIVATE KEY-----\ninvalid\n-----END PRIVATE KEY-----"}) {
		t.Fatal("private key bundle accepted")
	}
	for _, cfg := range []Config{{WatchAdoptionPlan: "plan"}, {WatchAdoptionPlan: "plan", Upgrade: true}, {WatchAdoptionPlan: "plan", Upgrade: true, SkipArgo: true, InstallRekor: true}} {
		if e := cfg.Validate(); e == nil {
			t.Fatalf("unsafe lifecycle option accepted: %+v", cfg)
		}
	}
}

func TestInstallOberthAdoptsReviewedConnectorBeforeHelmAndRetainsOnHelmFailure(t *testing.T) {
	cfg, deps, c, p, _ := watchFixture(t)
	cfg.ChartPath = "reviewed-chart"
	render := deps.RunHelm
	helmMutations := 0
	deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return render(ctx, args)
		case "upgrade":
			if strings.Contains(strings.Join(args, " "), "--dry-run=server") {
				return render(ctx, args)
			}
			if strings.Contains(strings.Join(args, " "), "--take-ownership") {
				t.Fatal("actual Helm upgrade bypassed ownership")
			}
			helmMutations++
			// 4 metadata CAS patches + 2 ConfigMap data field ownership
			// transfers (#813) = 6 patches before the Helm upgrade.
			if watchPatchCount(c) != 6 {
				t.Fatal("Helm ran before complete metadata and data field adoption")
			}
			return nil, errors.New("simulated Helm failure")
		default:
			t.Fatalf("unexpected Helm operation %v", args)
			return nil, nil
		}
	}
	result, err := InstallOberth(context.Background(), cfg, deps, OpenBaoResult{}, RekorResult{})
	if err == nil || len(result.WatchAdoption) != 4 {
		t.Fatalf("Helm failure or public adoption receipt lost: %+v %v", result, err)
	}
	if helmMutations != 1 {
		t.Fatal("Helm retried or not reached")
	}
	for _, o := range p.Objects {
		v, err := getWatchObject(context.Background(), deps, p.Namespace, o)
		if err != nil || watchOwner(v.meta).Release != "oberth" || !sameWatchJSON(v.spec, o.Spec) {
			t.Fatal("Helm failure rolled back or altered adoption")
		}
	}
}

func TestWatchAdoptionRejectsAuthorityOverrides(t *testing.T) {
	for _, change := range []string{"chart", "image", "policy", "version", "image-value", "policy-value", "unrelated-value", "duplicate-value", "scalar-value"} {
		t.Run(change, func(t *testing.T) {
			cfg, _, _, _, _ := watchFixture(t)
			var value string
			switch change {
			case "chart":
				cfg.ChartPath = "arbitrary"
			case "image":
				cfg.ImageRef = "arbitrary"
			case "policy":
				cfg.NetworkPolicy = "false"
			case "version":
				cfg.ChartVersion = ""
			case "image-value":
				value = "watchTunnel:\n  image: arbitrary\n"
			case "policy-value":
				value = "networkPolicy:\n  enabled: false\n"
			case "unrelated-value":
				value = "argo:\n  vault:\n    address: https://other.example\n"
			case "duplicate-value":
				value = "watchTunnel:\n  enabled: true\n  enabled: false\n"
			case "scalar-value":
				value = "watchTunnel: false\n"
			}
			if value != "" {
				p := filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(p, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.ValuesFiles = []string{p}
			}
			if err := cfg.Validate(); err == nil {
				t.Fatal("unreviewed authority override accepted")
			}
		})
	}
	cfg, _, _, _, _ := watchFixture(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), " ")
	if strings.Contains(args, "networkPolicy.enabled=") || !strings.Contains(args, "watchTunnel.image="+watchTunnelImageDefault) || !strings.Contains(args, "watchTunnel.openbaoImage="+watchTunnelOpenbaoImageDefault) {
		t.Fatal("adoption changes policy or loses image pins")
	}
}

func TestWatchPublicCARejectsSkippedPreambleAndMalformedFirstBlock(t *testing.T) {
	cert := string(secretWithCertificate(t, nil, nil).Data["tls.crt"])
	for _, prefix := range []string{"private arbitrary bytes\n", "-----BEGIN CERTIFICATE-----\nmalformed\n-----END CERTIFICATE-----\n", "-----BEGIN CERTIFICATE-----\nmalformed-without-end\n"} {
		if watchPublicCA(map[string]string{"ca.crt": prefix + cert}) {
			t.Fatal("noncertificate preamble or malformed first block accepted")
		}
	}
	if !watchPublicCA(map[string]string{"ca.crt": " \n" + cert + "\n" + cert}) {
		t.Fatal("valid public certificate bundle rejected")
	}
}

func TestExecuteWatchAdoptionNeverBootstrapsDarwin(t *testing.T) {
	cfg, _, _, _, _ := watchFixture(t)
	for _, dry := range []bool{false, true} {
		cfg.DryRun = dry
		deps := InstallDeps{GOOS: "darwin", RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("adoption attempted host bootstrap")
			return nil, nil
		}}
		if err := Execute(context.Background(), cfg, deps); err == nil {
			t.Fatal("unsupported platform adoption accepted")
		}
	}
}

func TestWatchPreviewRealSubprocessBoundsAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	args := []string{"upgrade", "--dry-run=server", "--no-hooks", "--take-ownership", "--output=json"}
	for _, script := range []string{"#!/bin/sh\nprintf 'private fixture output'\nprintf 'private fixture stderr' >&2\nexit 1\n", "#!/bin/sh\nhead -c 8388609 /dev/zero\n", "#!/bin/sh\nhead -c 65537 /dev/zero >&2\n"} {
		if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := DefaultRunHelm(ctx, args)
		cancel()
		if err == nil || len(out) != 0 || strings.Contains(err.Error(), "private fixture") {
			t.Fatalf("preview output bound or sanitization failed: bytes=%d err=%v", len(out), err)
		}
	}
}

func TestWatchAdoptionFinalCheckRejectsAlreadyOwnedDrift(t *testing.T) {
	cfg, deps, c, p, _ := watchFixture(t)
	// Last object is already adopted in a forward-repair plan.
	last := &p.Objects[3]
	last.Ownership = watchOwnership{"Helm", "oberth", "oberth"}
	obj, e := c.AppsV1().Deployments("oberth").Get(context.Background(), last.Name, metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	obj.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	obj.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}
	if e = c.Tracker().Update(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, obj, "oberth"); e != nil {
		t.Fatal(e)
	}
	writeWatchPlan(t, cfg, p)
	c.PrependReactor("patch", "serviceaccounts", func(ktesting.Action) (bool, runtime.Object, error) {
		obj.ResourceVersion = "999"
		if e := c.Tracker().Update(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, obj, "oberth"); e != nil {
			t.Fatal(e)
		}
		return false, nil, nil
	})
	receipt, err := adoptWatchTunnelReceipt(context.Background(), cfg, deps, false)
	if err == nil || len(receipt) != 3 || strings.Contains(err.Error(), "no adoption metadata changed") {
		t.Fatalf("final drift or confirmed receipt lost: %v %+v", err, receipt)
	}
}
