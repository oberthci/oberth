//go:build watch_real_api

package installer

// This deliberately uses the private production effect engine. It does not
// qualify the signed released-installer admission path or a live cutover.
import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	sigsyaml "sigs.k8s.io/yaml"
)

var realWatchConfig = flag.String("watch-fixture-config", "", "owned tmpfs synthetic client config")
var realWatchTools = flag.String("watch-fixture-tools", "", "admitted public tool binding")

type realWatchBinding struct {
	Helm       string `json:"helm"`
	HelmSHA    string `json:"helm_sha256"`
	Kubectl    string `json:"kubectl"`
	KubectlSHA string `json:"kubectl_sha256"`
	APIVersion string `json:"api_version"`
}

type realWatchPublicProof struct {
	Scenario          string         `json:"scenario"`
	Namespace         string         `json:"namespace"`
	OriginalConflicts []string       `json:"original_conflicts"`
	Confirmed         []watchAdopted `json:"confirmed_metadata_commits"`
	FailedRevision    int            `json:"failed_revision"`
	FailedStatus      string         `json:"failed_status"`
	FailedUID         string         `json:"failed_record_uid"`
	FailedRV          string         `json:"failed_record_resource_version"`
}

var realWatchPublicProofs []realWatchPublicProof

func realWatchPreserveOriginal(t *testing.T, ctx context.Context, d Deps, ns string, conflicts []string, confirmed []watchAdopted) {
	t.Helper()
	record, err := d.KubeClient.CoreV1().Secrets(ns).Get(ctx, "sh.helm.release.v1.oberth.v72", metav1.GetOptions{})
	if err != nil || record.Labels["status"] != "failed" || record.Labels["version"] != "72" {
		t.Fatal("original real failed72 state not established")
	}
	name := strings.TrimPrefix(t.Name(), "TestWatchRecoveryRealAPI/")
	realWatchPublicProofs = append(realWatchPublicProofs, realWatchPublicProof{Scenario: name, Namespace: ns, OriginalConflicts: conflicts, Confirmed: confirmed, FailedRevision: 72, FailedStatus: "failed", FailedUID: string(record.UID), FailedRV: record.ResourceVersion})
	raw, err := json.Marshal(map[string]any{"original_attempts": realWatchPublicProofs})
	cwd, e := os.Getwd()
	if err != nil || e != nil || os.WriteFile(filepath.Join(cwd, "public-original-attempts.json"), raw, 0600) != nil {
		t.Fatal("original public failure proof persistence failed")
	}
}

func realWatchDeps(t *testing.T) (context.Context, Deps, realWatchBinding) {
	t.Helper()
	uid := os.Getuid()
	groups, err := os.Getgroups()
	cwd, e := os.Getwd()
	var fs unix.Statfs_t
	var st unix.Stat_t
	if uid < 1000000000 || uid >= 2000000000 || os.Getgid() != uid || err != nil || len(groups) != 0 || e != nil || unix.Statfs(cwd, &fs) != nil || fs.Type != unix.TMPFS_MAGIC || unix.Lstat(cwd, &st) != nil || int(st.Uid) != uid || st.Mode&0777 != 0700 || filepath.Dir(*realWatchConfig) != cwd || filepath.Dir(*realWatchTools) != cwd {
		t.Fatal("real fixture requires admitted reserved tmpfs custody")
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil || unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}) != nil {
		t.Fatal("real fixture process custody refused")
	}
	raw, err := boundedWatchFile(*realWatchTools, 4096)
	var tools realWatchBinding
	if err != nil || strictWatchDecode(raw, &tools) != nil || filepath.Base(tools.Helm) != "helm" || watchBinaryDigestMatches(tools.Helm, tools.HelmSHA) != nil || watchBinaryDigestMatches(tools.Kubectl, tools.KubectlSHA) != nil {
		t.Fatal("admitted fixture tool binding differs")
	}
	raw, err = boundedWatchFile(*realWatchConfig, 65536)
	if err != nil {
		t.Fatal("owned synthetic kubeconfig unavailable")
	}
	defer clear(raw)
	k, err := clientcmd.Load(raw)
	if err != nil || k.CurrentContext != "watch-csa-isolated" || len(k.Clusters) != 1 || len(k.AuthInfos) != 1 || len(k.Contexts) != 1 {
		t.Fatal("synthetic config shape differs")
	}
	for _, c := range k.Clusters {
		if !strings.HasPrefix(c.Server, "https://127.0.0.1:") || c.InsecureSkipTLSVerify || c.CertificateAuthority != "" || len(c.CertificateAuthorityData) == 0 || c.ProxyURL != "" {
			t.Fatal("synthetic API trust differs")
		}
	}
	for _, a := range k.AuthInfos {
		if a.Exec != nil || a.AuthProvider != nil || a.Token != "" || a.TokenFile != "" || a.Username != "" || a.Password != "" || a.ClientCertificate != "" || a.ClientKey != "" || len(a.ClientCertificateData) == 0 || len(a.ClientKeyData) == 0 || a.Impersonate != "" || len(a.ImpersonateGroups) != 0 {
			t.Fatal("synthetic client credential roles differ")
		}
	}
	r, err := clientcmd.NewDefaultClientConfig(*k, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal("synthetic HTTPS client unavailable")
	}
	r.Timeout = 15 * time.Second
	c, err := kubernetes.NewForConfig(r)
	if err != nil {
		t.Fatal("synthetic API client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	d := Deps{KubeClient: c, RestConfig: r, ContextName: k.CurrentContext, Output: io.Discard}
	d.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		return runBoundedRecoveryHelm(ctx, tools.Helm, append(args, "--kubeconfig", *realWatchConfig, "--kube-context", k.CurrentContext))
	}
	v, err := c.Discovery().ServerVersion()
	if err != nil || !supportedWatchRecoveryAPI(v.GitVersion) || v.GitVersion != tools.APIVersion {
		t.Fatal("real API must match its exact admitted qualified patch version")
	}
	vraw, err := d.RunHelm(ctx, []string{"version", "--template", "{{.Version}}"})
	if err != nil || string(vraw) != "v4.2.3" {
		t.Fatal("real Helm must be exactly 4.2.3")
	}
	return ctx, d, tools
}

func watchBinaryDigestMatches(path, wanted string) error {
	d, err := watchBinaryDigest(path)
	if err != nil || d != wanted || len(wanted) != 64 {
		return errors.New("tool digest differs")
	}
	return nil
}

func realWatchChart(t *testing.T, version, manifest string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "chart")
	if os.MkdirAll(filepath.Join(dir, "templates"), 0700) != nil {
		t.Fatal("fixture chart preparation failed")
	}
	for name, raw := range map[string]string{"Chart.yaml": "apiVersion: v2\nname: oberth\nversion: " + version + "\n", "values.yaml": "{}\n", "templates/watch.yaml": manifest} {
		if os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600) != nil {
			t.Fatal("public fixture chart write failed")
		}
	}
	return dir
}

// Only this wholly synthetic, owned API rewrites its own public release record
// to seed the incident revision. Production never has this fixture operation.
func realWatchSeed71(t *testing.T, ctx context.Context, d Deps, ns string) {
	t.Helper()
	empty := realWatchChart(t, "0.16.24", "")
	if _, err := d.RunHelm(ctx, []string{"install", "oberth", empty, "-n", ns, "--no-hooks"}); err != nil {
		t.Fatal("synthetic public release installation failed")
	}
	s, err := d.KubeClient.CoreV1().Secrets(ns).Get(ctx, "sh.helm.release.v1.oberth.v1", metav1.GetOptions{})
	if err != nil {
		t.Fatal("synthetic release record absent")
	}
	b, err := base64.StdEncoding.DecodeString(string(s.Data["release"]))
	if err != nil {
		t.Fatal("synthetic release encoding differs")
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal("synthetic release compression differs")
	}
	b, err = io.ReadAll(io.LimitReader(z, 1<<20))
	_ = z.Close()
	var r map[string]any
	if err != nil || len(b) >= 1<<20 || !uniqueWatchJSON(b) || json.Unmarshal(b, &r) != nil {
		t.Fatal("synthetic public release shape differs")
	}
	r["version"] = 71
	b, err = json.Marshal(r)
	if err != nil {
		t.Fatal("synthetic release preparation failed")
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err = w.Write(b); err != nil {
		t.Fatal("synthetic release preparation failed")
	}
	if w.Close() != nil {
		t.Fatal("synthetic release preparation failed")
	}
	s.Name = "sh.helm.release.v1.oberth.v71"
	s.UID = ""
	s.ResourceVersion = ""
	s.ManagedFields = nil
	s.CreationTimestamp = metav1.Time{}
	s.Labels["version"] = "71"
	s.Data["release"] = []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))
	if _, err = d.KubeClient.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{FieldManager: "owned-public-fixture-seed"}); err != nil {
		t.Fatal("synthetic71 create failed")
	}
	if d.KubeClient.CoreV1().Secrets(ns).Delete(ctx, "sh.helm.release.v1.oberth.v1", metav1.DeleteOptions{}) != nil {
		t.Fatal("synthetic initial record retirement failed")
	}
}

func realWatchObjects(ns, ca string) []runtime.Object {
	off := false
	meta := func(name string) metav1.ObjectMeta {
		instance := "oberth"
		var annotations map[string]string
		if name == "cloudflared-watch-oberth-v2" {
			instance = name
			annotations = map[string]string{
				"deployment.kubernetes.io/revision": "2",
				"oberth.ci/source":                  "github.com/oberthci/terraform//k8s/cloudflared-watch",
			}
		}
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": "cloudflared-watch", "app.kubernetes.io/instance": instance}, Annotations: annotations}
	}
	return []runtime.Object{
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: meta("cloudflared-watch"), AutomountServiceAccountToken: &off},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("cloudflared-watch-openbao-ca"), Data: map[string]string{"ca.crt": ca}},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta("cloudflared-watch-oberth-origin-ca"), Data: map[string]string{"ca.crt": ca}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta("cloudflared-watch-oberth-v2"), Spec: appsv1.DeploymentSpec{Replicas: new(int32), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "owned-csa-fixture"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "owned-csa-fixture"}}, Spec: corev1.PodSpec{ServiceAccountName: "cloudflared-watch", AutomountServiceAccountToken: &off, Containers: []corev1.Container{{Name: "cloudflared", Image: "example.invalid/public@sha256:" + strings.Repeat("a", 64)}}, InitContainers: []corev1.Container{{Name: "fetch-token", Image: "example.invalid/public-bao@sha256:" + strings.Repeat("b", 64), Args: []string{"public-original-args"}}}}}}},
	}
}

func realWatchManifest(t *testing.T, objects []runtime.Object, original bool, chartVersion, ns string) string {
	t.Helper()
	var out strings.Builder
	for _, obj := range objects {
		x := obj.DeepCopyObject()
		v, err := watchSpec(x)
		if err != nil {
			t.Fatal("public fixture object invalid")
		}
		labels := v.meta.GetLabels()
		labels["app.kubernetes.io/managed-by"] = "Helm"
		if !original {
			version := canonicalChartVersion(chartVersion)
			if version == "" {
				t.Fatal("invalid real fixture chart version")
			}
			labels["helm.sh/chart"] = "oberth-" + strings.ReplaceAll(strings.TrimPrefix(version, "v"), "+", "_")
		}
		v.meta.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": ns})
		if dep, ok := x.(*appsv1.Deployment); ok {
			dep.Spec.Template.Spec.InitContainers[0].Args = []string{"public-successor-args"}
		}
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatal("public fixture serialization failed")
		}
		y, err := sigsyaml.JSONToYAML(b)
		if err != nil {
			t.Fatal("public fixture serialization failed")
		}
		if original {
			// Literal original duplicate labels: last YAML occurrence wins in Helm.
			y = bytes.Replace(y, []byte("    app.kubernetes.io/name: cloudflared-watch\n"), []byte("    app.kubernetes.io/name: cloudflared-watch\n    app.kubernetes.io/name: oberth\n"), 1)
			if _, ok := x.(*appsv1.Deployment); ok {
				y = bytes.Replace(y, []byte("    app.kubernetes.io/instance: cloudflared-watch-oberth-v2\n"), []byte("    app.kubernetes.io/instance: cloudflared-watch-oberth-v2\n    app.kubernetes.io/instance: oberth\n"), 1)
			}
		}
		out.Write(y)
		out.WriteString("---\n")
	}
	return out.String()
}

func realWatchTargets(t *testing.T, ctx context.Context, cfg Config, d Deps) []runtime.Object {
	t.Helper()
	raw, err := d.RunHelm(ctx, append(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), "--dry-run=server", "--output=json", "--no-hooks", "--take-ownership"))
	if err != nil {
		t.Fatal("synthetic real Helm preview failed")
	}
	var p struct {
		Manifest string `json:"manifest"`
	}
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("synthetic real Helm envelope invalid")
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(p.Manifest), 4096)
	var objects []runtime.Object
	for {
		var body json.RawMessage
		err = decoder.Decode(&body)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal("synthetic original manifest invalid")
		}
		if len(body) == 0 || bytes.Equal(body, []byte("null")) {
			continue
		}
		var id struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(body, &id) != nil {
			t.Fatal("fixture identity invalid")
		}
		obj := emptyRecoveryObject(id.Kind)
		if obj == nil || json.Unmarshal(body, obj) != nil {
			t.Fatal("fixture inventory invalid")
		}
		objects = append(objects, obj)
	}
	if len(objects) != 4 {
		t.Fatal("fixture requires four real Helm targets")
	}
	return objects
}

// Bit positions mirror the production conflict predicates, without projecting
// any API text. The last control for each scenario uses the corrected target.
func realWatchConflictPredicates(err error, o watchRecoveryObject) int {
	flags := 0
	set := func(bit int, ok bool) {
		if ok {
			flags |= 1 << bit
		}
	}
	set(0, apierrors.IsConflict(err))
	var status apierrors.APIStatus
	set(1, errors.As(err, &status))
	if status == nil {
		return flags
	}
	s := status.Status()
	set(2, s.Code == 409)
	set(3, s.Details != nil)
	if s.Details == nil {
		return flags
	}
	set(4, s.Details.Name == "" || s.Details.Name == o.Name)
	set(5, s.Details.Kind == "" || s.Details.Kind == "deployments")
	set(6, s.Details.Group == "" || s.Details.Group == "apps")
	set(7, len(s.Details.Causes) == 1)
	set(8, o.Kind == "Deployment")
	set(9, o.Name == "cloudflared-watch-oberth-v2")
	if len(s.Details.Causes) != 1 {
		return flags
	}
	c := s.Details.Causes[0]
	set(10, c.Type == metav1.CauseTypeFieldManagerConflict)
	set(11, c.Field == watchArgsField)
	set(12, c.Message == watchLegacyConflictMessage(o.ManagedFields))
	return flags
}

func realWatchConflicts(t *testing.T, ctx context.Context, d Deps, ns string, targets []runtime.Object) []string {
	t.Helper()
	var fields []string
	for _, obj := range targets {
		v, err := watchSpec(obj)
		if err != nil {
			t.Fatal("fixture target invalid")
		}
		current, err := getWatchObject(ctx, d, ns, watchAdoptionObject{Kind: obj.GetObjectKind().GroupVersionKind().Kind, Name: v.meta.GetName()})
		if err != nil {
			t.Fatal("fixture observation failed")
		}
		v.meta.SetUID(current.meta.GetUID())
		v.meta.SetResourceVersion(current.meta.GetResourceVersion())
		raw, _ := json.Marshal(obj)
		force := false
		_, err = patchRecoveryObject(ctx, d, ns, obj.GetObjectKind().GroupVersionKind().Kind, v.meta.GetName(), types.ApplyPatchType, raw, metav1.PatchOptions{FieldManager: "helm", Force: &force, DryRun: []string{metav1.DryRunAll}, FieldValidation: "Strict"})
		if err == nil {
			continue
		}
		if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			bound := watchRecoveryObject{Kind: "Deployment", Name: v.meta.GetName(), ManagedFields: current.meta.GetManagedFields()}
			t.Logf("WATCH_CONFLICT %s predicates=%d", strings.TrimPrefix(t.Name(), "TestWatchRecoveryRealAPI/"), realWatchConflictPredicates(err, bound))
		}
		status, ok := err.(apierrors.APIStatus)
		if !ok || !apierrors.IsConflict(err) || status.Status().Code != 409 || status.Status().Details == nil {
			t.Fatal("real SSA control returned unstructured failure")
		}
		for _, c := range status.Status().Details.Causes {
			if c.Type != metav1.CauseTypeFieldManagerConflict || !strings.HasPrefix(c.Message, `conflict with "kubectl-client-side-apply" using `) {
				t.Fatal("real SSA control has unrelated owner")
			}
			fields = append(fields, obj.GetObjectKind().GroupVersionKind().Kind+"/"+v.meta.GetName()+c.Field)
		}
	}
	sort.Strings(fields)
	return fields
}

func realWatchFailedScenario(t *testing.T, ctx context.Context, d Deps, tools realWatchBinding) (Config, []runtime.Object) {
	t.Helper()
	ns, err := d.KubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "oberth-csa-owned-"}}, metav1.CreateOptions{FieldManager: "owned-fixture"})
	if err != nil {
		t.Fatal("owned isolated namespace create failed")
	}
	realWatchSeed71(t, ctx, d, ns.Name)
	objects := realWatchObjects(ns.Name, string(d.RestConfig.CAData))
	var manifest strings.Builder
	for _, o := range objects {
		b, _ := json.Marshal(o)
		manifest.Write(b)
		manifest.WriteString("\n---\n")
	}
	input := filepath.Join(t.TempDir(), "public-csa.yaml")
	if os.WriteFile(input, []byte(manifest.String()), 0600) != nil {
		t.Fatal("public CSA preparation failed")
	}
	// Actual kubectl client-side apply creates its last-applied annotation and
	// Update ownership. No managedFields are injected to manufacture this state.
	if _, err = runBoundedRecoveryHelm(ctx, tools.Kubectl, []string{"--kubeconfig", *realWatchConfig, "apply", "--server-side=false", "-f", input, "-n", ns.Name}); err != nil {
		t.Fatal("genuine CSA fixture creation failed")
	}
	oldChart := realWatchChart(t, "0.16.25", realWatchManifest(t, objects, true, "v0.16.25", ns.Name))
	cfg := Config{Namespace: ns.Name, ChartVersion: "v0.16.25", ChartPath: oldChart, Upgrade: true, WatchAdoptionPlan: filepath.Join(t.TempDir(), "public-v1-plan.json")}
	targets := realWatchTargets(t, ctx, cfg, d)
	got := realWatchConflicts(t, ctx, d, ns.Name, targets)
	want := []string{"ConfigMap/cloudflared-watch-oberth-origin-ca.metadata.labels.app.kubernetes.io/name", "ConfigMap/cloudflared-watch-openbao-ca.metadata.labels.app.kubernetes.io/name", "Deployment/cloudflared-watch-oberth-v2.metadata.labels.app.kubernetes.io/instance", "Deployment/cloudflared-watch-oberth-v2.metadata.labels.app.kubernetes.io/name", "Deployment/cloudflared-watch-oberth-v2" + watchArgsField, "ServiceAccount/cloudflared-watch.metadata.labels.app.kubernetes.io/name"}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatal("original six SSA conflicts not reproduced exactly")
	}
	p := watchAdoptionPlan{Schema: watchAdoptionSchema, Namespace: ns.Name, ChartVersion: cfg.ChartVersion, ReleaseRevision: 71, ExpiresAt: time.Now().Add(15 * time.Minute)}
	for _, target := range targets {
		tv, _ := watchSpec(target)
		kind := target.GetObjectKind().GroupVersionKind().Kind
		v, err := getWatchObject(ctx, d, ns.Name, watchAdoptionObject{Kind: kind, Name: tv.meta.GetName()})
		if err != nil {
			t.Fatal("CSA baseline unavailable")
		}
		p.Objects = append(p.Objects, watchAdoptionObject{Kind: kind, Name: tv.meta.GetName(), UID: string(v.meta.GetUID()), ResourceVersion: v.meta.GetResourceVersion(), Ownership: watchOwner(v.meta), Spec: v.spec, TargetSpec: tv.spec})
	}
	writeWatchPlan(t, cfg, p)
	receipt, err := adoptWatchTunnelReceipt(ctx, cfg, d, false)
	if err != nil || len(receipt) != 4 {
		t.Fatal("real original four metadata CAS not reproduced")
	}
	if _, err = d.RunHelm(ctx, OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})); err == nil {
		t.Fatal("original normal Helm unexpectedly succeeded")
	}
	realWatchPreserveOriginal(t, ctx, d, ns.Name, got, receipt)
	corrected := realWatchChart(t, "0.16.26", realWatchManifest(t, objects, false, "v0.16.26", ns.Name))
	cfg.ChartVersion = "v0.16.26"
	cfg.ChartPath = ""
	cfg.watchChart = corrected
	return cfg, realWatchTargets(t, ctx, cfg, d)
}

func realWatchPlan(t *testing.T, ctx context.Context, cfg Config, d Deps, targets []runtime.Object) watchRecoveryPlan {
	t.Helper()
	v, err := d.KubeClient.Discovery().ServerVersion()
	if err != nil {
		t.Fatal("API version unavailable")
	}
	cluster, err := d.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal("isolated cluster UID unavailable")
	}
	record, err := d.KubeClient.CoreV1().Secrets(cfg.Namespace).Get(ctx, "sh.helm.release.v1.oberth.v72", metav1.GetOptions{})
	if err != nil {
		t.Fatal("actual failed72 record absent")
	}
	p := watchRecoveryPlan{Schema: watchRecoverySchema, Mode: "failed-adoption-recovery", Namespace: cfg.Namespace, Context: d.ContextName, ClusterUID: string(cluster.UID), ServerVersion: v.GitVersion, ChartVersion: cfg.ChartVersion, CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(15 * time.Minute), Release: watchReleaseGuard{Revision: 72, Status: "failed", Chart: "oberth-0.16.25", RecordUID: string(record.UID), RecordRV: record.ResourceVersion, PreviousRevision: 71}}
	for _, target := range targets {
		tv, _ := watchSpec(target)
		kind := target.GetObjectKind().GroupVersionKind().Kind
		observed, err := getWatchObject(ctx, d, cfg.Namespace, watchAdoptionObject{Kind: kind, Name: tv.meta.GetName()})
		if err != nil {
			t.Fatal("failed scenario observation unavailable")
		}
		tv.meta.SetUID("")
		tv.meta.SetResourceVersion("")
		render, _ := json.Marshal(target)
		defaulted := observed.meta.(runtime.Object).DeepCopyObject()
		defaulted.GetObjectKind().SetGroupVersionKind(target.GetObjectKind().GroupVersionKind())
		dv, _ := watchSpec(defaulted)
		dv.meta.SetManagedFields(nil)
		dv.meta.SetResourceVersion("")
		if dep, ok := defaulted.(*appsv1.Deployment); ok {
			dep.Spec.Template.Spec.InitContainers[0].Args = []string{"public-successor-args"}
			dep.Generation++
		}
		def, _ := json.Marshal(defaulted)
		def = recoveryTargetWithChartLabel(t, def, p.ChartVersion)
		p.Objects = append(p.Objects, watchRecoveryObject{Kind: kind, Name: tv.meta.GetName(), UID: string(observed.meta.GetUID()), ResourceVersion: observed.meta.GetResourceVersion(), Generation: observed.meta.GetGeneration(), Metadata: watchPublicMetadata{observed.meta.GetLabels(), observed.meta.GetAnnotations()}, ManagedFields: observed.meta.GetManagedFields(), Spec: observed.spec, Target: render, DefaultedTarget: def})
	}
	return p
}

func writeRealResumeArtifact(t *testing.T, value any) watchPublicFile {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("resume evidence serialization failed")
	}
	path := filepath.Join(t.TempDir(), "resume-evidence.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal("resume evidence write failed")
	}
	sum := sha256.Sum256(raw)
	return watchPublicFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

// Only fixed source literals receive diagnostic IDs. Unknown errors stay opaque;
// API/Helm output and arbitrary error strings never enter the public receipt.
func realWatchErrorCode(err error) int {
	if err == nil {
		return 0
	}
	fixed := []string{
		"watch recovery original attempt binding differs",
		"original watch plan scope differs",
		"original watch execution receipt differs",
		"watch recovery is not bound to original confirmed UIDs",
		"watch recovery context or deadline differs",
		"watch recovery cluster UID differs",
		"watch recovery exact API version differs",
		"cannot establish exact latest Helm history",
		"watch recovery latest failed history moved",
		"watch recovery requires real metadata API observation",
		"cannot observe failed Helm record metadata",
		"failed Helm record identity or public state moved",
		"watch recovery expired during observation",
		"watch recovery Helm preview failed",
		"watch preview release identity differs",
		"watch preview YAML invalid",
		"duplicate or invalid watch preview YAML",
		"watch preview object identity invalid",
		"duplicate watch preview object",
		"watch preview typed object invalid",
		"foreign watch preview ownership",
		"watch recovery render differs from reviewed target",
		"watch recovery preview inventory differs",
		"watch signed recovery plan absent",
		"watch recovery requires prepared signed target and public receipt output",
		"watch Deployment missing",
		"watch metadata handoff dry-run differs from reviewed subtraction",
		"watch handoff outcome uncertain for Deployment/cloudflared-watch-oberth-v2; retain state, no automatic retry or rollback",
		"watch handoff confirmed but public receipt output failed; stop and retain state",
		"watch recovery real SSA qualification incomplete",
		"invalid or duplicate public JSON",
		"invalid public JSON shape",
		"invalid public watch plan path",
		"invalid public watch plan",
		"invalid v2 public watch plan",
		"v2 watch scope, API version or deadline differs",
		"v2 watch public values digest is required",
		"watch recovery is limited to the reviewed failed adoption revision",
		"unsupported watch recovery mode",
		"invalid v2 watch object",
		"unapproved watch metadata label",
		"unapproved watch annotation",
		"unapproved last-applied watch annotation",
		"invalid public last-applied watch object",
		"unapproved last-applied watch spec",
		"unsupported watch target",
		"invalid public watch target",
		"watch target identity or ownership differs",
		"watch target contains unsupported non-label metadata",
		"rendered target contains unrelated API metadata",
		"rendered target contains API identity",
		"defaulted target must bind UID and omit variable RV",
		"unapproved target metadata",
		"unsupported watch patch",
		"cannot encode watch apply",
		"watch apply did not report only the reviewed legacy args conflict",
		"watch force=false apply preflight failed",
		"invalid watch SSA response",
		"watch SSA defaulted public target differs",
		"cannot observe watch object",
		"watch current complete metadata differs from approved public target identity",
		"foreign watch ownership",
		"failed recovery requires confirmed Helm ownership",
		"invalid legacy field trie",
		"empty legacy field trie",
		"unapproved legacy field trie shape",
		"atomic ancestor owns watch args",
		"invalid watch args ownership",
		"watch args field is not an atomic leaf",
		"watch ownership entry count differs",
		"unknown watch ownership entry",
		"duplicate watch ownership entry",
		"legacy watch owner identity differs",
		"shared or foreign watch args ownership",
		"sole legacy watch args owner not established",
		"watch handoff would leave an empty ownership ancestor",
		"cannot encode watch handoff",
		"invalid entry",
		"unsupported watch metadata",
		"watch recovery state differs for ServiceAccount/cloudflared-watch",
		"watch recovery state differs for ConfigMap/cloudflared-watch-openbao-ca",
		"watch recovery state differs for ConfigMap/cloudflared-watch-oberth-origin-ca",
		"watch recovery state differs for Deployment/cloudflared-watch-oberth-v2",
		"coordinated cutover stopped after confirmed metadata commit",
	}
	for i, text := range fixed {
		if err.Error() == text {
			return i + 1
		}
	}
	return 999
}

func TestWatchRecoveryRealAPI(t *testing.T) {
	ctx, d, tools := realWatchDeps(t)
	for _, scenario := range []string{"same-UID-normal-Helm", "shared-owner", "extra-conflict", "all-four-RV", "expired", "failed-record-RV", "post-handoff-stop"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, targets := realWatchFailedScenario(t, ctx, d, tools)
			if got := realWatchConflicts(t, ctx, d, cfg.Namespace, targets); len(got) != 1 || got[0] != "Deployment/cloudflared-watch-oberth-v2"+watchArgsField {
				t.Fatal("corrected labels must leave precisely one intended conflict")
			}
			// Restore clean target metadata after the read-only control added UID/RV.
			targets = realWatchTargets(t, ctx, cfg, d)
			p := realWatchPlan(t, ctx, cfg, d, targets)
			cfg.watchRecovery = &p
			var receipt bytes.Buffer
			local := d
			local.Output = &receipt
			guard := func(p watchRecoveryPlan) error { return requireRecoveryGuard(ctx, local, p) }
			switch scenario {
			case "shared-owner":
				force := false
				b, _ := json.Marshal(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "cloudflared-watch-oberth-v2", "namespace": cfg.Namespace}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"initContainers": []any{map[string]any{"name": "fetch-token", "args": []string{"public-original-args"}}}}}}})
				if _, err := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Patch(ctx, "cloudflared-watch-oberth-v2", types.ApplyPatchType, b, metav1.PatchOptions{FieldManager: "foreign-shared-control", Force: &force}); err != nil {
					t.Fatal("real shared owner control failed")
				}
				p = realWatchPlan(t, ctx, cfg, d, targets)
				cfg.watchRecovery = &p
			case "extra-conflict":
				var additional strings.Builder
				for i := range p.Objects {
					if p.Objects[i].Kind == "Deployment" {
						obj, err := recoveryTarget(p.Objects[i], false, p.Namespace)
						if err != nil {
							t.Fatal("target preparation failed")
						}
						obj.(*appsv1.Deployment).Spec.Template.Spec.InitContainers[0].Image = "example.invalid/foreign@sha256:" + strings.Repeat("c", 64)
						p.Objects[i].Target, _ = json.Marshal(obj)
					}
					additional.Write(p.Objects[i].Target)
					additional.WriteString("\n---\n")
				}
				cfg.watchChart = realWatchChart(t, "0.16.26", additional.String())
			case "all-four-RV":
				cm, err := d.KubeClient.CoreV1().ConfigMaps(cfg.Namespace).Get(ctx, "cloudflared-watch-openbao-ca", metav1.GetOptions{})
				if err != nil {
					t.Fatal("RV control unavailable")
				}
				cm.Annotations["owned-fixture-drift"] = "public"
				if _, err = d.KubeClient.CoreV1().ConfigMaps(cfg.Namespace).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "owned-drift"}); err != nil {
					t.Fatal("RV drift control failed")
				}
			case "expired":
				p.ExpiresAt = time.Now().Add(-time.Second)
			case "failed-record-RV":
				s, err := d.KubeClient.CoreV1().Secrets(cfg.Namespace).Get(ctx, "sh.helm.release.v1.oberth.v72", metav1.GetOptions{})
				if err != nil {
					t.Fatal("failed record control unavailable")
				}
				s.Annotations = map[string]string{"owned-drift": "public"}
				if _, err = d.KubeClient.CoreV1().Secrets(cfg.Namespace).Update(ctx, s, metav1.UpdateOptions{FieldManager: "owned-drift"}); err != nil {
					t.Fatal("failed record drift control failed")
				}
			case "post-handoff-stop":
				originalRV := ""
				for _, o := range p.Objects {
					if o.Kind == "Deployment" {
						originalRV = o.ResourceVersion
					}
				}
				guard = func(candidate watchRecoveryPlan) error {
					for _, o := range candidate.Objects {
						if o.Kind == "Deployment" && o.ResourceVersion != originalRV {
							return errors.New("coordinated cutover stopped after confirmed metadata commit")
						}
					}
					return requireRecoveryGuard(ctx, local, candidate)
				}
			}
			priorPlan := p
			before, err := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, "cloudflared-watch-oberth-v2", metav1.GetOptions{})
			if err != nil {
				t.Fatal("handoff baseline unavailable")
			}
			if before.Annotations["deployment.kubernetes.io/revision"] != "2" || before.Annotations["oberth.ci/source"] != "github.com/oberthci/terraform//k8s/cloudflared-watch" {
				t.Fatal("real fixture did not retain the observed public deployment annotations")
			}
			beforeSpec, _ := json.Marshal(before.Spec)
			rows, err := applyWatchRecovery(ctx, cfg, local, false, guard)
			t.Logf("WATCH_ENGINE %s code=%d receipts=%d ready=%t", scenario, realWatchErrorCode(err), len(rows), cfg.watchRecovery.ready)
			expectedRefusal := map[string]string{
				"shared-owner":      "watch apply did not report only the reviewed legacy args conflict",
				"extra-conflict":    "watch apply did not report only the reviewed legacy args conflict",
				"all-four-RV":       "watch recovery state differs for ConfigMap/cloudflared-watch-openbao-ca",
				"expired":           "watch recovery context or deadline differs",
				"failed-record-RV":  "failed Helm record identity or public state moved",
				"post-handoff-stop": "coordinated cutover stopped after confirmed metadata commit",
			}
			if want, negative := expectedRefusal[scenario]; negative && (err == nil || err.Error() != want) {
				t.Fatal("negative did not reach its intended refusal category")
			}
			if scenario != "same-UID-normal-Helm" && scenario != "post-handoff-stop" {
				if err == nil || len(rows) != 0 {
					t.Fatal("real negative reached a confirmed handoff")
				}
				after, e := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, before.Name, metav1.GetOptions{})
				if e != nil || after.ResourceVersion != before.ResourceVersion || !sameWatchManaged(after.ManagedFields, before.ManagedFields) {
					t.Fatal("negative modified ownership")
				}
				return
			}
			if len(rows) != 1 || !strings.Contains(receipt.String(), "legacy args ownership relinquished") || scenario == "post-handoff-stop" && err == nil || scenario == "same-UID-normal-Helm" && err != nil {
				t.Fatal("real engine result or retained receipt differs")
			}
			after, err := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, before.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal("handoff readback unavailable")
			}
			afterSpec, _ := json.Marshal(after.Spec)
			_, expected, e := watchArgsHandoff(before.ManagedFields)
			if err != nil || e != nil || after.UID != before.UID || after.Generation != before.Generation || !maps.Equal(after.Annotations, before.Annotations) || !sameWatchJSON(beforeSpec, afterSpec) || !sameWatchManaged(after.ManagedFields, expected) {
				t.Fatal("actual API metadata CAS changed spec, generation or unrelated ownership")
			}
			if scenario == "post-handoff-stop" {
				if cfg.watchRecovery.ready {
					t.Fatal("stopped handoff became Helm-ready")
				}
				// Build a fresh, hash-bound continuation from the exact prior plan
				// and the one-row receipt just emitted by the confirmed CAS.
				priorPlan.CreatedAt, priorPlan.ExpiresAt = time.Now().Add(-20*time.Minute), time.Now().Add(-time.Minute)
				priorPlan.Origin = &watchRecoveryOrigin{Plan: watchPublicFile{Path: "synthetic-original-plan", SHA256: strings.Repeat("a", 64)}, Receipt: watchPublicFile{Path: "synthetic-original-receipt", SHA256: strings.Repeat("b", 64)}, Confirmed: []watchAdopted{}}
				cfg.ChartVersion = "v0.16.27"
				objects := realWatchObjects(cfg.Namespace, string(d.RestConfig.CAData))
				cfg.watchChart = realWatchChart(t, "0.16.27", realWatchManifest(t, objects, false, cfg.ChartVersion, cfg.Namespace))
				freshTargets := realWatchTargets(t, ctx, cfg, local)
				fresh := realWatchPlan(t, ctx, cfg, local, freshTargets)
				fresh.ValuesSHA256 = priorPlan.ValuesSHA256
				fresh.CreatedAt, fresh.ExpiresAt = time.Now().Add(-time.Second), time.Now().Add(15*time.Minute)
				fresh.Origin = priorPlan.Origin
				priorArtifact := writeRealResumeArtifact(t, priorPlan)
				receiptArtifact := writeRealResumeArtifact(t, rows)
				fresh.Mode = "post-handoff-resume"
				fresh.Resume = &watchRecoveryResume{PriorPlan: priorArtifact, HandoffReceipt: receiptArtifact}
				if err = validateRecoveryResume(fresh); err != nil {
					t.Fatalf("valid real post-handoff resume refused: %v", err)
				}
				resumeRV, resumeFields := after.ResourceVersion, after.ManagedFields
				cfg.watchRecovery = &fresh
				var resumeReceipt bytes.Buffer
				local.Output = &resumeReceipt
				resumed, resumeErr := applyWatchRecovery(ctx, cfg, local, false, func(candidate watchRecoveryPlan) error { return requireRecoveryGuard(ctx, local, candidate) })
				if resumeErr != nil || len(resumed) != 1 || !cfg.watchRecovery.ready || !strings.Contains(resumeReceipt.String(), "confirmed handoff resumed") {
					t.Fatalf("post-handoff resume failed: rows=%v ready=%t err=%v", resumed, cfg.watchRecovery.ready, resumeErr)
				}
				afterResume, readErr := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, before.Name, metav1.GetOptions{})
				if readErr != nil || afterResume.ResourceVersion != resumeRV || !sameWatchManaged(afterResume.ManagedFields, resumeFields) {
					t.Fatal("read-only resume changed persisted Deployment metadata")
				}
				rows, err = resumed, resumeErr
			}
			if err = requireWatchRecoveryBeforeHelm(ctx, cfg, local); err != nil {
				t.Fatal("final real production guard failed")
			}
			if _, err = d.RunHelm(ctx, OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})); err != nil {
				t.Fatal("ordinary Helm same-UID forward convergence failed")
			}
			for _, o := range p.Objects {
				current, e := getWatchObject(ctx, d, p.Namespace, watchAdoptionObject{Kind: o.Kind, Name: o.Name})
				wanted, e2 := recoveryTarget(o, true, p.Namespace)
				if e != nil || e2 != nil {
					t.Fatal("convergence observation unavailable")
				}
				wv, _ := watchSpec(wanted)
				if string(current.meta.GetUID()) != o.UID || !sameWatchJSON(current.spec, wv.spec) {
					t.Fatal("normal Helm replaced an object or diverged from reviewed target")
				}
			}
			final, e := d.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, before.Name, metav1.GetOptions{})
			if e != nil {
				t.Fatal("final ownership unavailable")
			}
			if !maps.Equal(final.Annotations, before.Annotations) {
				t.Fatal("ordinary Helm changed the retained public annotations in the controller-free fixture")
			}
			legacy := func(entries []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
				var rows []metav1.ManagedFieldsEntry
				for _, x := range entries {
					if x.Manager == watchLegacyManager {
						rows = append(rows, x)
					}
				}
				return rows
			}
			if !sameWatchManaged(legacy(final.ManagedFields), legacy(expected)) {
				t.Fatal("ordinary Helm changed unrelated legacy ownership")
			}
			helmOwns := false
			for _, x := range final.ManagedFields {
				if x.Manager == "helm" && x.Operation == metav1.ManagedFieldsOperationApply && x.FieldsV1 != nil {
					tree, e := decodeWatchTrie(x.FieldsV1.Raw)
					if e == nil {
						helmOwns, _ = ownsWatchArgs(tree)
					}
				}
			}
			if !helmOwns {
				t.Fatal("ordinary Helm did not acquire the intended args field")
			}
			raw, e := d.RunHelm(ctx, []string{"history", "oberth", "-n", p.Namespace, "--max", "1", "-o", "json"})
			var history []helmRelease
			if e != nil || json.Unmarshal(raw, &history) != nil || len(history) != 1 || string(history[0].Revision) != "73" && string(history[0].Revision) != `"73"` || history[0].Status != "deployed" {
				t.Fatal("normal Helm did not finish deployed73")
			}
		})
	}
}
