package argoworkflow

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

// Covers: REQ-C4-012
func TestGrantedMountPathsContract(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/var/run/secrets/secretstore", "/var/run/secrets/kubernetes.io/serviceaccount", "/run/oberth", "/var"} {
		bad := wf.DeepCopy()
		bad.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "replace", Path: root, ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "replace"}}}}
		requireCode(t, admitFixture(t.Context(), bad, p), "path")
	}
	wf.Spec.Templates[0].Volumes = append(wf.Spec.Templates[0].Volumes, corev1.Volume{Name: "private", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "ungranted"}}}})
	requireCode(t, admitFixture(t.Context(), wf, p), "read")
}

// Covers: REQ-C4-013
func TestServerStateBoundaryContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, code string
		change     func(*wfv1.Workflow)
	}{
		{"server volume", "reserved", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "oberth-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{"executor volume", "reserved", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "var-run-argo", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{"server pvc", "pvc", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "oberth-data"}}}}
		}},
		{"host filesystem", "volume", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}},
		{"host namespace", "host", func(w *wfv1.Workflow) { w.Spec.HostNetwork = ptr.To(true) }},
		{"privileged default", "host", func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := simpleWorkflow()
			p := Policy{PVCClaims: map[string]bool{"oberth-data": true}}
			if _, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks()); err != nil {
				t.Fatal(err)
			}
			tc.change(w)
			_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
			requireCode(t, err, tc.code)
		})
	}
}

// Supports: REQ-C4-014 (the grant walk verifies the CLI path)
func TestGrantRemedyMakesSamePushAdmissible(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	declareSecretPaths(&wf.Spec.Templates[0], "public/key")
	wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	err := admitFixture(t.Context(), wf, p)
	requireCode(t, err, "grant")
	var refused *AdmissionError
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	const command = "oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'main' 'public/key' < <value-file>"
	if commandForFinding(t, refused) != command || !strings.Contains(err.Error(), command) {
		t.Fatalf("command: %q", commandForFinding(t, refused))
	}
	args := strings.Split(commandForFinding(t, refused), "'")
	p.Grants = append(p.Grants, admissioncontract.GrantEvidence{Repository: args[1], File: args[3], Step: args[5], Kind: "secret", Path: args[7], Revision: "operator-approval", State: admissioncontract.StateVerified})
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal("printed correction did not work", err)
	}
}
func TestTemplateCertificateFailureNamesItsFix(t *testing.T) {
	t.Parallel()
	p := admissionFixture{Policy: Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) {
		return nil, x509.UnknownAuthorityError{}
	}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}},
	}
	err := admitFixture(t.Context(), sharedConsumer("shared", "work"), p)
	requireCode(t, err, "certificate")
	if strings.Contains(err.Error(), "Retry when") {
		t.Fatal("certificate failure described as transient")
	}
}
func TestNativeFieldSpellingIsCaseSensitive(t *testing.T) {
	t.Parallel()
	raw := string(mustDocument(t, simpleWorkflow()))
	for _, field := range []string{"apiVersion", "kind", "spec", "entrypoint", "templates", "container", "image"} {
		bad := strings.Replace(raw, `"`+field+`"`, `"`+strings.ToUpper(field[:1])+field[1:]+`"`, 1)
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("case variant %s accepted", field)
		}
	}
	wf := simpleWorkflow()
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "leaf", Inline: &wfv1.Template{Container: &corev1.Container{Image: "image"}}}}}}}
	raw = string(mustDocument(t, wf))
	raw = strings.Replace(raw, `"inline"`, `"Inline"`, 1)
	if _, err := Decode([]byte(raw)); err == nil {
		t.Fatal("case variant in custom native carrier accepted")
	}
}
func TestNativeFreedomsHaveNoDefaultRefusal(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.PodPriorityClassName = "custom"
	wf.Spec.Templates[0].PriorityClassName = "custom"
	wf.Spec.SecurityContext = &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "kernel.hostname", Value: "custom"}}}
	wf.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "LABELS", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}}}}
	wf.Spec.Templates[0].ArchiveLocation = &wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "value"}}
	wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.ArtifactGC.PodSpecPatch = "{}"
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "patch")
}

func TestPreparationPlanContract(t *testing.T) {
	t.Parallel()
	r := requestFor(t, simpleWorkflow())
	prepared, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.View().Plan.Exact || len(prepared.View().Plan.Entries) != 1 {
		t.Fatal("leaf plan absent")
	}
	raw, err := json.Marshal(prepared.View().Plan)
	if err != nil || strings.Contains(string(raw), "Template") {
		t.Fatal("private template escaped", err)
	}
}

// Supports: REQ-C4-011 (the push walk verifies the CLI path)
func TestRefusalsNameTheSelectedFile(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.PodSpecPatch = "{}"
	r := requestFor(t, wf)
	_, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
	requireCode(t, err, "patch")
	if !strings.Contains(err.Error(), r.SelectedFile) {
		t.Fatal("file missing from refusal", err)
	}
}

func TestSharedTemplateAbsenceAndPermissionExplainTheirFix(t *testing.T) {
	t.Parallel()
	wf := sharedConsumer("shared", "work")
	for _, tc := range []struct {
		code  string
		cause error
	}{{"template_missing", apierrors.NewNotFound(schema.GroupResource{Resource: "workflowtemplates"}, "shared")}, {"template_permission", apierrors.NewForbidden(schema.GroupResource{Resource: "workflowtemplates"}, "shared", errors.New("not allowed"))}} {
		p := admissionFixture{Policy: Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) { return nil, tc.cause }}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
		requireCode(t, admitFixture(t.Context(), wf, p), tc.code)
	}
}
func TestSharedTemplateYAMLErrorDoesNotSuggestRetryingTheNetwork(t *testing.T) {
	t.Parallel()
	p := admissionFixture{Policy: Policy{TemplateGetter: func(context.Context, string) (*wfv1.WorkflowTemplate, error) {
		return nil, &json.SyntaxError{Offset: 1}
	}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}},
	}
	requireCode(t, admitFixture(t.Context(), sharedConsumer("shared", "work"), p), "template_yaml")
}
func TestInlineGrantRemedyUsesTheAcceptedScope(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	leaf := wf.Spec.Templates[0]
	leaf.Container.Command = []string{"oberth", "secretstore", "exec"}
	declareSecretPaths(&leaf, "public/key")
	p.Grants[0].Step = "*"
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "inline", Inline: &leaf}}}}
	err := admitFixture(t.Context(), wf, p)
	var refused *AdmissionError
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	args := strings.Split(commandForFinding(t, refused), "'")
	if len(args) != 9 || args[5] != "*" {
		t.Fatal("inline remedy does not use wildcard grant", commandForFinding(t, refused))
	}
	p.Grants = append(p.Grants, admissioncontract.GrantEvidence{Repository: args[1], File: args[3], Step: args[5], Kind: "secret", Path: args[7], Revision: "operator-approval", State: admissioncontract.StateVerified})
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal("inline remedy did not admit the same YAML", err)
	}
}

func commandForFinding(t *testing.T, e *AdmissionError) string {
	t.Helper()
	var commands []string
	for _, f := range e.Findings {
		if f.Command != "" {
			commands = append(commands, f.Command)
		}
	}
	if len(commands) != 1 || e.Detail != "" {
		t.Fatalf("one typed command required: %+v", e)
	}
	return commands[0]
}
