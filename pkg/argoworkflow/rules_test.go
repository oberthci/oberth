package argoworkflow

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestOrdinaryArtifactsLabelsAndProjectionsPrepare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*wfv1.Workflow)
	}{
		{"http", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "tool", Path: "/tool", ArtifactLocation: wfv1.ArtifactLocation{HTTP: &wfv1.HTTPArtifact{URL: "https://example.test/tool"}}}}
		}},
		{"git", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "dependency", Path: "/dependency", ArtifactLocation: wfv1.ArtifactLocation{Git: &wfv1.GitArtifact{Repo: "https://example.test/dependency.git"}}}}
		}},
		{"s3", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "data", Path: "/data", ArtifactLocation: wfv1.ArtifactLocation{S3: &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{Endpoint: "objects.example.test", Bucket: "data", Insecure: ptr.To(false)}, Key: "input"}}}}
		}},
		{"labels", func(w *wfv1.Workflow) {
			w.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"app.kubernetes.io/name": "app", "app.kubernetes.io/instance": "ci", "app.kubernetes.io/component": "build", "component": "build", "kubernetes.io/metadata.name": "ci"}, Annotations: map[string]string{"kubectl.kubernetes.io/default-container": "main"}}
		}},
		{"projection", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "metadata", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}}}}}}}
		}},
		{"downward", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "metadata", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}}}
		}},
		{"container freedoms", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"ALL", "SYS_ADMIN"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}, ProcMount: ptr.To(corev1.UnmaskedProcMount)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := simpleWorkflow()
			tc.change(w)
			if _, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestClusterSecretSelectorsRequireExactStepEvidence(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"http", "git", "s3", "volume", "projection", "configmapProjection", "env", "envFrom", "imagePullSecrets", "configmap"} {
		t.Run(location, func(t *testing.T) {
			w := simpleWorkflow()
			selector := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}, Key: "token"}
			kind := clusterLocation(w, selector, location)
			r := requestFor(t, w)
			_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
			requireCode(t, err, "read")
			r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", kind, "private")}
			if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err != nil {
				t.Fatal("grant did not admit the same document", err)
			}
			for _, field := range []string{"repo", "file", "step", "path", "kind", "state", "revision"} {
				wrong := r.Clone()
				g := &wrong.Grants[0]
				wrongGrant(g, field)
				_, err := Prepare(t.Context(), wrong, Policy{}, identityHooks())
				requireCode(t, err, "read")
			}
		})
	}
}
func TestVolumeSourceAllowlistIsClosed(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[corev1.VolumeSource]()
	allowed := map[string]bool{"csi": true, "emptyDir": true, "downwardAPI": true, "projected": true, "secret": true, "configMap": true, "persistentVolumeClaim": true}
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if allowed[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			w := simpleWorkflow()
			v := corev1.VolumeSource{}
			reflect.ValueOf(&v).Elem().Field(i).Set(reflect.New(field.Type.Elem()))
			w.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: v}}
			err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks())
			for _, f := range admissionFindings(t, err) {
				if f.Code == "volume" && f.Path == "spec.volumes[data]."+name && strings.Contains(f.Remedy, "granted PVC") {
					return
				}
			}
			t.Fatal(err)
		})
	}
}
func TestResourceGrantsCannotCreateUnadmittedWorkloadsOrReachProtectedNamespaces(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"argoproj.io/v1alpha1", "argoproj.io/v1beta1"} {
		for _, kind := range []string{"Workflow", "CronWorkflow", "WorkflowTemplate", "ClusterWorkflowTemplate"} {
			t.Run(kind+"/"+version, func(t *testing.T) {
				w, p := resourceWorkflow("apiVersion: " + version + "\nkind: " + kind + "\nmetadata: {name: child, namespace: resources}")
				p.Capability.ResourceKinds = []ResourceKind{{APIVersion: version, Kind: kind, Actions: []string{"create", "get", "delete", "apply"}}}
				requireCode(t, p.admit(t.Context(), w, requestFor(t, w), identityHooks()), "workload_kind")
			})
		}
	}
	for _, namespace := range []string{"server-system", "workflow-system"} {
		t.Run(namespace, func(t *testing.T) {
			w, p := resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: deploy, namespace: " + namespace + "}")
			p.ServerNamespace = "server-system"
			p.ArgoNamespace = "workflow-system"
			p.Capability.Namespace = namespace
			requireCode(t, p.admit(t.Context(), w, requestFor(t, w), identityHooks()), "protected_namespace")
		})
	}
	w, p := resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: deploy, namespace: application}")
	p.Capability.Namespace = "application"
	r := requestFor(t, w)
	r.IdentityRequest.ResourceAccount = "resources"
	if _, err := Prepare(t.Context(), r, p.Policy, identityHooks()); err != nil {
		t.Fatal("deployment in a granted namespace", err)
	}
	p.ArgoNamespace = ""
	_, err := Prepare(t.Context(), r, p.Policy, identityHooks())
	requireCode(t, err, "namespace_policy")
}
func TestSourceAndReviewedRawArtifactMayOverlap(t *testing.T) {
	t.Parallel()
	w := sourceWorkflow()
	w.Spec.Templates[0].Inputs.Artifacts = append(w.Spec.Templates[0].Inputs.Artifacts, wfv1.Artifact{Name: "authored", Path: "/work/src/main.go", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "reviewed source"}}})
	if err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks()); err != nil {
		t.Fatal(err)
	}
}
func TestUnknownFieldsNameAllExactJSONPaths(t *testing.T) {
	t.Parallel()
	raw := `{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","container":{"image":"image","securityContext":{"Privileged":true,"Unexpected":1}}},{"name":"other","container":{"image":"image","wrongB":2}}]}}`
	_, err := Decode([]byte(raw))
	findings := admissionFindings(t, err)
	want := []string{"spec.templates[0].container.securityContext.Privileged", "spec.templates[0].container.securityContext.Unexpected", "spec.templates[1].container.wrongB"}
	if len(findings) != len(want) {
		t.Fatal(findings)
	}
	for i, f := range findings {
		if f.Path != want[i] || f.Code != "unknown_field" || f.Reason != "unknown field; field names are case-sensitive" || f.Remedy != "Use the exact native spelling of this field." {
			t.Fatal(f)
		}
	}
}
func TestRefusalMessagesDescribeTheirCauses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, code, reason, remedy string
		change                     func(*wfv1.Workflow)
		policy                     Policy
	}{
		{"registry", "image", "image is outside the allowed registries", "Use an image under an allowed prefix or ask the operator to allow this registry.", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = "other.test/image" }, Policy{ImagePrefixes: []string{"example.test/"}}},
		{"http", "http", "HTTP templates are not supported", "Call the endpoint from a container step instead.", func(w *wfv1.Workflow) { w.Spec.Templates[0].HTTP = &wfv1.HTTP{URL: "https://example.test"} }, Policy{}},
		{"plugin", "plugin", "plugin templates are not supported", "Call the endpoint from a container step instead.", func(w *wfv1.Workflow) { w.Spec.Templates[0].Plugin = &wfv1.Plugin{} }, Policy{}},
		{"typo", "local_template", "template is not defined in this file", `Correct the template name; closest existing template: "main".`, func(w *wfv1.Workflow) { w.Spec.Entrypoint = "mian" }, Policy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := simpleWorkflow()
			tc.change(w)
			err := tc.policy.admit(t.Context(), w, requestFor(t, w), identityHooks())
			for _, f := range admissionFindings(t, err) {
				if f.Code == tc.code && f.Reason == tc.reason && f.Remedy == tc.remedy {
					return
				}
			}
			t.Fatal(err)
		})
	}
}
func TestNativeErrorAppearsOnceWithPathAndDetail(t *testing.T) {
	t.Parallel()
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks[0].Arguments.Parameters = []wfv1.Parameter{{Name: "missing-value"}}
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	findings := admissionFindings(t, err)
	if len(findings) != 1 || findings[0].Path == "spec" {
		t.Fatal("native error path or count", findings)
	}
	var refused *AdmissionError
	if !errors.As(err, &refused) || !strings.Contains(refused.Detail, "missing-value") || strings.Contains(refused.Detail, "\n") {
		t.Fatal("native detail duplicated or lost", err)
	}
}

func clusterLocation(w *wfv1.Workflow, selector *corev1.SecretKeySelector, location string) string {
	c := w.Spec.Templates[0].Container
	kind := "cluster-secret"
	switch location {
	case "http":
		w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/input", ArtifactLocation: wfv1.ArtifactLocation{HTTP: &wfv1.HTTPArtifact{URL: "https://example.test/input", Auth: &wfv1.HTTPAuth{BasicAuth: wfv1.BasicAuth{UsernameSecret: selector}}}}}}
	case "git":
		w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/input", ArtifactLocation: wfv1.ArtifactLocation{Git: &wfv1.GitArtifact{Repo: "https://example.test/input.git", UsernameSecret: selector}}}}
	case "s3":
		w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/input", ArtifactLocation: wfv1.ArtifactLocation{S3: &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{Endpoint: "objects.example.test", Bucket: "data", AccessKeySecret: selector}, Key: "input"}}}}
	case "volume":
		w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: selector.Name}}}}
	case "projection":
		w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: selector.LocalObjectReference}}}}}}}
	case "env":
		c.Env = []corev1.EnvVar{{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: selector}}}
	case "envFrom":
		c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: selector.LocalObjectReference}}}
	case "imagePullSecrets":
		kind = "image-pull-secret"
		w.Spec.ImagePullSecrets = []corev1.LocalObjectReference{selector.LocalObjectReference}
	case "configmapProjection":
		kind = "configmap"
		w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: selector.LocalObjectReference}}}}}}}
	case "configmap":
		kind = "configmap"
		w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: selector.LocalObjectReference}}}}
	}
	return kind
}

func wrongGrant(g *admissioncontract.GrantEvidence, field string) {
	switch field {
	case "repo":
		g.Repository = "forge/org/other"
	case "file":
		g.File = ".oberth/release.yaml"
	case "step":
		g.Step = "other"
	case "path":
		g.Path = "other"
	case "kind":
		g.Kind = "other"
	case "state":
		g.State = admissioncontract.StateUnknown
	case "revision":
		g.Revision = ""
	}
}

func TestSingleNativeWorkflowHookErrorIsDeterministic(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Hooks = wfv1.LifecycleHooks{"failed": {Template: "main"}, "running": {Template: "main", Expression: "workflow.status == 'Running'"}}
	var first string
	for range 3 {
		_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
		findings := admissionFindings(t, err)
		if len(findings) != 1 || findings[0].Path != "spec.hooks[failed]" {
			t.Fatal(findings)
		}
		if first != "" && first != err.Error() {
			t.Fatal("native findings changed order")
		}
		first = err.Error()
	}
}

func TestOnlySourcePluginOwnsSourceRoots(t *testing.T) {
	t.Parallel()
	for _, location := range []wfv1.ArtifactLocation{
		{HTTP: &wfv1.HTTPArtifact{URL: "https://example.test/input"}},
		{Git: &wfv1.GitArtifact{Repo: "https://example.test/dependency.git"}},
		{S3: &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{Endpoint: "objects.example.test", Bucket: "data"}, Key: "input"}},
		{Raw: &wfv1.RawArtifact{Data: "authored"}},
	} {
		for _, destination := range []string{"/work/src", "/work/sources/pin", "/work/other/../src"} {
			w := simpleWorkflow()
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "dependency", Path: "/dependency", ArtifactLocation: location}}
			if _, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks()); err != nil {
				t.Fatal("ordinary artifact refused", err)
			}
			w.Spec.Templates[0].Inputs.Artifacts[0].Path = destination
			_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
			requireCode(t, err, "source")
		}
	}
}
