package argoworkflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func sourceWorkflow() *wfv1.Workflow {
	wf := simpleWorkflow()
	wf.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{mainSource()}
	wf.Spec.Templates[0].Container.WorkingDir = "/work/src"
	return wf
}
func tokenWorkflow() (*wfv1.Workflow, admissionFixture) {
	wf := sourceWorkflow()
	tpl := &wf.Spec.Templates[0]
	tpl.Volumes = []corev1.Volume{{Name: "login", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "login", ExpirationSeconds: ptr.To(int64(600)), Path: "token"}}}}}}}
	tpl.Container.VolumeMounts = []corev1.VolumeMount{{Name: "login", MountPath: "/var/run/secrets/secretstore", ReadOnly: true}}
	p := admissionFixture{PrepareHooks: admissioncontract.PrepareHooks{LoginAudience: "login"}, PrepareRequest: admissioncontract.PrepareRequest{Repository: "forge/org/repo", SelectedFile: ".oberth/build.yaml", Grants: []admissioncontract.GrantEvidence{{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: "main", Kind: "identity", Path: "login", Audience: "login", Revision: "1", State: admissioncontract.StateVerified}}}}
	return wf, p
}

// Covers: REQ-C4-001
func TestExactGrantedSourceAndSecretPathsContract(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"repository", "file", "step", "audience", "state", "revision"} {
		t.Run(field, func(t *testing.T) {
			bad := p
			bad.Grants = append([]admissioncontract.GrantEvidence{}, p.Grants...)
			grant := &bad.Grants[0]
			switch field {
			case "repository":
				grant.Repository = "forge/other/repo"
			case "file":
				grant.File = ".oberth/release.yaml"
			case "step":
				grant.Step = "other"
			case "path":
				grant.Path = "other"
			case "audience":
				grant.Audience = "other"
			case "state":
				grant.State = admissioncontract.StateUnknown
			case "revision":
				grant.Revision = ""
			}
			requireCode(t, admitFixture(t.Context(), wf, bad), "grant")
		})
	}
	assertOutputExposureAccepted(t, wf, p)

	good := wf.DeepCopy()
	good.Spec.Templates[0].Outputs.Parameters = []wfv1.Parameter{{Name: "value", ValueFrom: &wfv1.ValueFrom{Path: "/work/value"}}}
	if err := admitFixture(t.Context(), good, p); err != nil {
		t.Fatal(err)
	}
	raw := wf.DeepCopy()
	raw.Spec.Templates[0].Inputs.Artifacts[0].Plugin = nil
	raw.Spec.Templates[0].Inputs.Artifacts[0].Raw = &wfv1.RawArtifact{Data: "raw"}
	requireCode(t, admitFixture(t.Context(), raw, p), "source")
}
func TestAuthoredCredentialProjectionShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*wfv1.Template)
	}{
		{"lifetime_short", func(t *wfv1.Template) {
			t.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(int64(599))
		}},
		{"lifetime_long", func(t *wfv1.Template) {
			t.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(int64(3601))
		}},
		{"traversal", func(t *wfv1.Template) { t.Volumes[0].Projected.Sources[0].ServiceAccountToken.Path = "../token" }},
		{"writable", func(t *wfv1.Template) { t.Container.VolumeMounts[0].ReadOnly = false }},
		{"missing_mount", func(t *wfv1.Template) { t.Container.VolumeMounts = nil }},
		{"double_mount", func(t *wfv1.Template) {
			t.Container.VolumeMounts = append(t.Container.VolumeMounts, t.Container.VolumeMounts[0])
		}},
		{"subpath", func(t *wfv1.Template) { t.Container.VolumeMounts[0].SubPath = "token" }},
		{"helper_mount", func(t *wfv1.Template) {
			t.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Image: "image", Name: "side", VolumeMounts: t.Container.VolumeMounts}}}
		}},
		{"helper_mirror", func(t *wfv1.Template) {
			t.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Image: "image", Name: "side"}, MirrorVolumeMounts: ptr.To(true)}}
		}},
		{"device", func(t *wfv1.Template) {
			t.Container.VolumeDevices = []corev1.VolumeDevice{{Name: "login", DevicePath: "/dev/token"}}
		}},
		{"multiple_sources", func(t *wfv1.Template) {
			t.Volumes[0].Projected.Sources = append(t.Volumes[0].Projected.Sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, p := tokenWorkflow()
			tc.change(&wf.Spec.Templates[0])
			requireCode(t, admitFixture(t.Context(), wf, p), "token")
		})
	}
}
func TestSourceUsageForwardingAndMixedOrigins(t *testing.T) {
	t.Parallel()
	wf := sourceWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	work.Inputs.Artifacts[0].Plugin = nil
	wf.Spec.Templates = []wfv1.Template{{Name: "main", Inputs: wfv1.Inputs{Artifacts: []wfv1.Artifact{mainSource()}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "work", Arguments: wfv1.Arguments{Artifacts: []wfv1.Artifact{{Name: "source", From: "{{inputs.artifacts.source}}"}}}}}}}, work}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"{{tasks.other.outputs.artifacts.source}}", "{{inputs.artifacts.missing}}"} {
		bad := wf.DeepCopy()
		bad.Spec.Templates[0].DAG.Tasks[0].Arguments.Artifacts[0].From = from
		requireCode(t, admitFixture(t.Context(), bad, admissionFixture{}), "source_usage")
	}
	mixed := wf.DeepCopy()
	mixed.Spec.Templates[0].DAG.Tasks = append(mixed.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "other", Template: "work"})
	requireCode(t, admitFixture(t.Context(), mixed, admissionFixture{}), "source_usage")
	// An unreachable invocation contributes no working-directory source requirement.

	unused := wf.DeepCopy()
	unused.Spec.Templates = append(unused.Spec.Templates, wfv1.Template{Name: "unused", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "other", Template: "work"}}}})
	if err := admitFixture(t.Context(), unused, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
}
func TestSourceUsageDefaultsAndOwnVolume(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{WorkingDir: "/work/src/sub"}}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "source_usage")
	wf.Spec.Volumes = []corev1.Volume{{Name: "own", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	wf.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "own", MountPath: "/work/src"}}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	command := simpleWorkflow()
	command.Spec.Templates[0].Container.Command = []string{"cat", "/work/src/file"}
	if err := admitFixture(t.Context(), command, admissionFixture{}); err != nil {
		t.Fatal("command text inferred source", err)
	}
}
func TestSourcePathsDescriptorsAndPins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, code string
		change     func(*wfv1.Artifact)
	}{
		{"key", "source", func(a *wfv1.Artifact) { a.Plugin.Key = "literal" }},
		{"descriptor", "source", func(a *wfv1.Artifact) { a.Plugin.Configuration = `{"version":1,"kind":"main","other":1}` }},
		{"duplicate_descriptor", "source", func(a *wfv1.Artifact) { a.Plugin.Configuration = `{"version":1,"kind":"main","kind":"main"}` }},
		{"path", "source", func(a *wfv1.Artifact) { a.Path = "/other" }},
		{"mode", "source", func(a *wfv1.Artifact) { a.Mode = ptr.To(int32(0777)) }},
		{"protected_ancestor", "path", func(a *wfv1.Artifact) { a.Path = "/var" }},
		{"dynamic", "path", func(a *wfv1.Artifact) { a.Path = "/x/{{inputs.parameters.path}}" }},
		{"external_backend", "artifact", func(a *wfv1.Artifact) { a.S3 = &wfv1.S3Artifact{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := sourceWorkflow()
			tc.change(&wf.Spec.Templates[0].Inputs.Artifacts[0])
			requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), tc.code)
		})
	}
	pins := simpleWorkflow()
	for i := range 5 {
		a := mainSource()
		a.Name = string(rune('a' + i))
		a.Path = "/work/sources/" + a.Name
		d := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "forge/org/repo", Commit: strings.Repeat("a", 40)}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		a.Plugin.Configuration = string(raw)
		pins.Spec.Templates[0].Inputs.Artifacts = append(pins.Spec.Templates[0].Inputs.Artifacts, a)
	}
	requireLimit(t, admitFixture(t.Context(), pins, admissionFixture{}), "pin_count", "four")
	pins.Spec.Templates[0].Inputs.Artifacts = pins.Spec.Templates[0].Inputs.Artifacts[:4]
	if err := admitFixture(t.Context(), pins, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
}

// Supports: REQ-C4-011 (the push walk verifies the CLI path)
func TestSourceResultReferencesRefuseAtAdmission(t *testing.T) {
	t.Parallel()
	for _, reference := range []string{"{{tasks.checkout.outputs.result}}", "{{=tasks['checkout'].outputs.result}}", "{{=tasks.checkout['outputs']['result']}}", "{{=tasks ['checkout'] . outputs . result}}", "{{=tasks[\"checkout\"]\n.outputs.result}}"} {
		wf := simpleWorkflow()
		work := wf.Spec.Templates[0]
		work.Name = "checkout"
		work.Inputs.Artifacts = []wfv1.Artifact{mainSource()}
		wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "checkout", Template: "checkout"}, {Name: "use", Template: "use", Arguments: wfv1.Arguments{Parameters: []wfv1.Parameter{{Name: "value", Value: wfv1.AnyStringPtr(reference)}}}}}}}, work, {Name: "use", Container: &corev1.Container{Image: "image"}}}
		err := admitFixture(t.Context(), wf, admissionFixture{})
		requireCode(t, err, "source_result")
		if !strings.Contains(err.Error(), "result of a source step is not readable; Write the value to a file and use an output parameter.") {
			t.Fatal(err)
		}
		wf.Spec.Templates[0].DAG.Tasks[1].Arguments.Parameters[0].Value = wfv1.AnyStringPtr("{{tasks.checkout.outputs.parameters.value}}")
		if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestSourceSpecSizeCountsSpecOnly(t *testing.T) {
	t.Parallel()
	wf := sourceWorkflow()
	wf.Spec.Templates[0].Container.Args = []string{strings.Repeat("a", admissioncontract.MaxSourceSpecBytes)}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "source_size")
	wf.Spec.Templates[0].Container.Args = nil
	wf.Annotations = map[string]string{"example.test/large": strings.Repeat("a", admissioncontract.MaxSourceSpecBytes)}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	plain := simpleWorkflow()
	plain.Spec.Templates[0].Container.Args = []string{strings.Repeat("a", admissioncontract.MaxSourceSpecBytes)}
	if err := admitFixture(t.Context(), plain, admissionFixture{}); err != nil {
		t.Fatal("spec cap applied without source", err)
	}
}

func TestSecretPathsAndLoginAreExact(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	declareSecretPaths(&wf.Spec.Templates[0], "public/key")
	wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	p.Grants = append(p.Grants, admissioncontract.GrantEvidence{Repository: p.Repository, File: p.SelectedFile, Step: "main", Kind: "secret", Path: "public/key", Revision: "1", State: admissioncontract.StateVerified})
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	for value, code := range map[string]string{"other/key": "grant", "public/../key": "secret_path", "/public/key": "secret_path", "public/key,public/key": "secret_path"} {
		bad := wf.DeepCopy()
		declareSecretPaths(&bad.Spec.Templates[0], value)
		requireCode(t, admitFixture(t.Context(), bad, p), code)
	}
	bad := wf.DeepCopy()
	bad.Spec.Templates[0].Volumes = nil
	bad.Spec.Templates[0].Container.VolumeMounts = nil
	if err := admitFixture(t.Context(), bad, p); err != nil {
		t.Fatal("native execution supplies the login projection", err)
	}
	p.LoginAudience = ""
	requireCode(t, admitFixture(t.Context(), bad, p), "store_config")
}
func TestTokenAudienceCanBeAnExactURL(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	p.Grants[0].Path = "public/identity"
	p.Grants[0].Audience = "https://identity.example.test"
	p.LoginAudience = p.Grants[0].Audience
	wf.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = p.Grants[0].Audience
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience += "/other"
	requireCode(t, admitFixture(t.Context(), wf, p), "grant")
}
func TestReleaseSelectionRequiresTagButNoIdentityMapping(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	p := submitPolicy()
	p.Release = true
	r := requestFor(t, wf)
	requireCode(t, prepareWithPolicy(t, r, p), "grant")
	r.Trigger = "tag"
	if err := prepareWithPolicy(t, r, p); err != nil {
		t.Fatal(err)
	}
}
func prepareWithPolicy(t *testing.T, r admissioncontract.PrepareRequest, p admissionFixture) error {
	t.Helper()
	_, err := prepareFixture(t.Context(), r, p, identityHooks())
	return err
}

func assertOutputExposureAccepted(t *testing.T, wf *wfv1.Workflow, p admissionFixture) {
	t.Helper()
	for _, value := range []string{"/var", "/var/run/secrets/secretstore/token", "{{inputs.parameters.file}}"} {
		t.Run("output_"+value, func(t *testing.T) {
			bad := wf.DeepCopy()
			bad.Spec.Templates[0].Outputs.Parameters = []wfv1.Parameter{{Name: "value", ValueFrom: &wfv1.ValueFrom{Path: value}}}
			if err := admitFixture(t.Context(), bad, p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceMountBoundaryIncludesContainerSetMounts(t *testing.T) {
	t.Parallel()
	for _, set := range []bool{false, true} {
		t.Run(fmt.Sprint(set), func(t *testing.T) {
			w := sourceWorkflow()
			tpl := &w.Spec.Templates[0]
			tpl.Volumes = []corev1.Volume{{Name: "tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			mount := corev1.VolumeMount{Name: "tools", MountPath: "/tools"}
			if set {
				tpl.Container = nil
				tpl.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "work", Image: "image"}}}, VolumeMounts: []corev1.VolumeMount{mount}}
			} else {
				tpl.Container.VolumeMounts = []corev1.VolumeMount{mount}
			}
			if err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks()); err != nil {
				t.Fatal(err)
			}
			if set {
				tpl.ContainerSet.VolumeMounts[0].MountPath = "/work/src/tools"
			} else {
				tpl.Container.VolumeMounts[0].MountPath = "/work/src/tools"
			}
			requireCode(t, (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks()), "path")
		})
	}
}

func TestInspectStillChecksStoreUnlessValidationIsOffline(t *testing.T) {
	wf := simpleWorkflow()
	declareSecretPaths(&wf.Spec.Templates[0], "public/key")
	wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Repository: "origin/team/widget", SelectedFile: ".oberth/build.yaml"}
	policy := Policy{}
	requireCode(t, policy.admit(t.Context(), wf, request, admissioncontract.PrepareHooks{}), "store_config")
	for _, hooks := range []admissioncontract.PrepareHooks{{OfflineValidation: true}, {LoginAudience: "store"}} {
		if err := policy.admit(t.Context(), wf, request, hooks); err != nil {
			t.Fatal("offline or configured inspection refused", err)
		}
	}
}
