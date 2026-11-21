package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func simpleWorkflow() *wfv1.Workflow {
	return &wfv1.Workflow{TypeMeta: metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow"}, Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "example.test/image:1", Command: []string{"true"}}}}}}
}
func mainSource() wfv1.Artifact {
	return wfv1.Artifact{Name: "source", Path: "/work/src", Mode: ptr.To(int32(0755)), ArtifactLocation: wfv1.ArtifactLocation{Plugin: &wfv1.PluginArtifact{Name: "oberth", Key: SourceArtifactKey, Configuration: `{"version":1,"kind":"main"}`}}}
}
func admissionFindings(t *testing.T, err error) []admissioncontract.Finding {
	t.Helper()
	var refused *AdmissionError
	if !errors.As(err, &refused) {
		t.Fatalf("expected AdmissionError, got %v", err)
	}
	return refused.Findings
}

// declareSecretPaths sets a step's own secret declaration.
func declareSecretPaths(t *wfv1.Template, paths string) {
	if t.Metadata.Annotations == nil {
		t.Metadata.Annotations = map[string]string{}
	}
	t.Metadata.Annotations[SecretPathsAnnotation] = paths
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	for _, f := range admissionFindings(t, err) {
		if f.Code == code {
			return
		}
	}
	t.Fatalf("missing %s: %v", code, err)
}
func requestFor(t *testing.T, wf *wfv1.Workflow) admissioncontract.PrepareRequest {
	t.Helper()
	document, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	blob := strings.Repeat("b", 40)
	revision := sourcecontract.Revision{Repository: "forge/org/repo", Commit: sha}
	return admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: revision.Repository, RunID: "run-1", Ref: "refs/heads/topic", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", FileBlob: blob, Document: document, SourceReceipt: sourcecontract.Receipt{Version: 1, Revision: revision, ObjectSHA: sha, File: ".oberth/build.yaml", Blob: blob, DocumentDigest: sourcecontract.Digest(document)}, IdentityRequest: admissioncontract.IdentityRequest{Mode: admissioncontract.ModeSubmit, Namespace: "ci", PipelineAccount: "ci-step"}}
}
func identityHooks() admissioncontract.PrepareHooks {
	return admissioncontract.PrepareHooks{VerifyIdentities: func(_ context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
		e := admissioncontract.IdentityEvidence{Namespace: r.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: r.PipelineAccount, UID: "step-uid"}, State: admissioncontract.StateVerified}

		if r.ResourceAccount != "" {
			e.Resource = &admissioncontract.AccountEvidence{Name: r.ResourceAccount, UID: "resource-uid"}
		}
		return e, nil
	}}
}
func submitPolicy() admissionFixture {
	return admissionFixture{}
}

func TestDecodeIsStrict(t *testing.T) {
	t.Parallel()
	good := `{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","container":{"image":"image"}}]}}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unknown":       strings.Replace(good, `"image":"image"`, `"image":"image","invented":true`, 1),
		"duplicate":     strings.Replace(good, `"kind":"Workflow"`, `"kind":"Workflow","kind":"Workflow"`, 1),
		"wrong_kind":    strings.Replace(good, "Workflow", "WorkflowTemplate", 1),
		"wrong_version": strings.Replace(good, "v1alpha1", "v1", 1),
		"empty":         "", "null": "null", "multiple": good + "\n---\n{}", "after_end": "---\n" + good + "\n...\n{}", "malformed": "[", "alias": "a: &a [*a]",
		"oversized": strings.Repeat(" ", admissioncontract.MaxDocumentBytes+1),
		"deep":      strings.Repeat("[", 70) + strings.Repeat("]", 70),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(input)); err == nil {
				t.Fatal("accepted invalid document")
			}
		})
	}
}

// Supports: REQ-C4-011 (the push walk verifies the CLI path)
func TestPushAdmissionNamesEveryOffender(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	for _, name := range []string{"one", "two"} {
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: name, PodSpecPatch: `{"hostPID":true}`, Container: &corev1.Container{Image: "image", Env: []corev1.EnvVar{{Name: "ARGO_CONTROL", Value: "x"}}, SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}})
	}
	err := admitFixture(t.Context(), wf, admissionFixture{})
	findings := admissionFindings(t, err)
	if len(findings) != 6 {
		t.Fatalf("all offenders: %+v", findings)
	}
	for _, name := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if again := admitFixture(t.Context(), wf, admissionFixture{}); again.Error() != err.Error() {
		t.Fatal("unstable refusals")
	}
}
func TestBoundaryInEveryCarrier(t *testing.T) {
	t.Parallel()
	for _, carrier := range []string{"unused", "defaults", "inline_step", "inline_dag", "hook"} {
		t.Run(carrier, func(t *testing.T) {
			wf := simpleWorkflow()
			hostile := wfv1.Template{Name: "bad", Container: &corev1.Container{Image: "image", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}
			switch carrier {
			case "unused":
				wf.Spec.Templates = append(wf.Spec.Templates, hostile)
			case "defaults":
				wf.Spec.TemplateDefaults = &hostile
			case "inline_step":
				wf.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "bad", Inline: &hostile}}}}}
			case "inline_dag":
				wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "bad", Inline: &hostile}}}}
			case "hook":
				wf.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Template: "bad"}}
				wf.Spec.Templates = append(wf.Spec.Templates, hostile)
			}
			requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "host")
		})
	}
}
func TestBoundaryFieldsRefuse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, code string
		change     func(*wfv1.Workflow)
	}{
		{"host_network", "host", func(w *wfv1.Workflow) { w.Spec.HostNetwork = ptr.To(true) }},
		{"host_path", "volume", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}},
		{"privileged", "host", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		}},
		{"host_port", "host", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Ports = []corev1.ContainerPort{{HostPort: 80}} }},
		{"host_ip", "host", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Ports = []corev1.ContainerPort{{HostIP: "127.0.0.1"}}
		}},
		{"secret_env", "read", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}}
		}},
		{"map_env", "read", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{}}}
		}},
		{"map_parameter", "read", func(w *wfv1.Workflow) {
			w.Spec.Arguments.Parameters = []wfv1.Parameter{{Name: "X", ValueFrom: &wfv1.ValueFrom{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{}}}}
		}},
		{"owned_metadata", "reserved", func(w *wfv1.Workflow) { w.Labels = map[string]string{"oberth.ci/run": "other"} }},
		{"parameter", "reserved", func(w *wfv1.Workflow) { w.Spec.Arguments.Parameters = []wfv1.Parameter{{Name: "oberth-source"}} }},
		{"global_parameter", "reserved", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Outputs.Parameters = []wfv1.Parameter{{Name: "x", GlobalName: "oberth-trigger"}}
		}},
		{"executor_name", "reserved", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Name = "wait" }},
		{"executor_mount", "reserved", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "exec-sa-token", MountPath: "/data"}}
		}},
		{"dynamic_env", "reserved", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "{{inputs.parameters.name}}"}}
		}},
		{"whole_reference", "reference", func(w *wfv1.Workflow) { w.Spec.WorkflowTemplateRef = &wfv1.WorkflowTemplateRef{Name: "other"} }},
		{"patch", "patch", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = "{}" }},
		{"memoize", "read", func(w *wfv1.Workflow) { w.Spec.Templates[0].Memoize = &wfv1.Memoize{} }},
		{"agent_http", "http", func(w *wfv1.Workflow) { w.Spec.Templates[0].HTTP = &wfv1.HTTP{URL: "https://example.test"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := simpleWorkflow()
			tc.change(wf)
			requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), tc.code)
		})
	}
	if err := admitFixture(t.Context(), simpleWorkflow(), admissionFixture{}); err != nil {
		t.Fatal(err)
	}
}
func TestNativePodOptionsAndNoRestrictedFloor(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	s := &wf.Spec
	c := s.Templates[0].Container
	s.HostAliases = []corev1.HostAlias{{IP: "127.0.0.1", Hostnames: []string{"test"}}}
	s.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"1.1.1.1"}}
	s.DNSPolicy = ptr.To(corev1.DNSNone)
	s.SchedulerName = "custom"
	s.PodDisruptionBudget = &policyv1.PodDisruptionBudgetSpec{}
	s.Parallelism = ptr.To(int64(999999))
	s.Priority = ptr.To(int32(100))
	s.ActiveDeadlineSeconds = ptr.To(int64(10000))
	s.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0)), Sysctls: []corev1.Sysctl{{Name: "net/ipv4/tcp_rmem", Value: "4096 131072 6291456"}}, SELinuxOptions: &corev1.SELinuxOptions{Type: "custom"}}
	c.SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), AllowPrivilegeEscalation: ptr.To(true), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}
	c.Resources.Claims = []corev1.ResourceClaim{{Name: "device"}}
	c.Env = []corev1.EnvVar{{Name: "NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}}
	s.Templates[0].Metadata.Annotations = map[string]string{"example.test/profile": "custom"}
	before := wf.DeepCopy()
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, wf) {
		t.Fatal("admission changed YAML")
	}
}
func TestRefusalCatalogIsCompleteAndShort(t *testing.T) {
	t.Parallel()
	for code, m := range messages() {
		for _, field := range []struct {
			name, value string
			max         int
		}{{"reason", m.reason, 80}, {"remedy", m.remedy, 120}} {
			if utf8.RuneCountInString(field.value) > field.max || field.value == "" || strings.ContainsAny(field.value, "\n\r") || strings.HasSuffix(field.value, "...") || strings.HasSuffix(field.value, "…") {
				t.Fatalf("%s.%s: %q", code, field.name, field.value)
			}
		}
	}
	for _, e := range []interface {
		Reason() string
		Remedy() string
	}{&AdmissionError{}, &TemplateReadError{}} {
		if len(e.Reason()) > 80 || len(e.Remedy()) > 120 {
			t.Fatal(e)
		}
	}
}
func TestNilPublicInputsAndCanceledReads(t *testing.T) {
	t.Parallel()
	if admitFixture(t.Context(), nil, admissionFixture{}) == nil {
		t.Fatal("nil admission")
	}
	if _, err := planFixture(t.Context(), nil, admissionFixture{}); err == nil {
		t.Fatal("nil planning")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	wf := sharedConsumer("shared", "work")
	if err := admitFixture(ctx, wf, admissionFixture{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOptionalNativeFieldsDoNotGainBlanketRefusals(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.ArchiveLogs = ptr.To(true)
	wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "own"}}}
	wf.Spec.Templates[0].Outputs.Artifacts = []wfv1.Artifact{{Name: "output", Path: "/work/value", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "value"}}}}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Outputs.Artifacts[0].Path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Outputs.Artifacts[0].Path = "/work/value"
	wf.Spec.Templates[0].Outputs.Artifacts[0].Raw = nil
	wf.Spec.Templates[0].Outputs.Artifacts[0].S3 = &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{AccessKeySecret: &corev1.SecretKeySelector{}}}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "read")
}

func TestStorageAndDownwardRoutesPreserveTheBoundary(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{SecretName: "private", ShareName: "data"}}}}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "volume")
	wf.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "storage.example.test", Path: "/data"}}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "volume")
	wf.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Volumes[0].DownwardAPI.Items[0].FieldRef.FieldPath = "metadata.annotations"
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Volumes = nil
	wf.Spec.Templates[0].Outputs.Parameters = []wfv1.Parameter{{Name: "token", ValueFrom: &wfv1.ValueFrom{Path: "/var/run/secrets/kubernetes.io/serviceaccount/token"}}}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeAcceptsBoundedYAMLAnchors(t *testing.T) {
	t.Parallel()
	raw := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: first
  templates:
    - name: first
      container: {image: &image example.test/image:1}
    - name: second
      container: {image: *image}
`)
	wf, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Templates[0].Container.Image != wf.Spec.Templates[1].Container.Image {
		t.Fatal("anchor value changed")
	}
}

func TestAdmissionErrorsKeepTheirDeterministicCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("invalid native input")
	err := documentError(cause)
	if !errors.Is(err, cause) {
		t.Fatal("deterministic cause lost")
	}
	if admissionFindings(t, err)[0].Code != "document" {
		t.Fatal("public refusal changed")
	}
}
