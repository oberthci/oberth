package argoworkflow

import (
	"context"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

// Twelve secret-route shapes in submit mode, with grants: admitted and never eligible.
func TestReviewTwelveSecretShapes(t *testing.T) {
	t.Parallel()
	sel := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}, Key: "k"}
	secretVol := corev1.Volume{Name: "s", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "private"}}}
	projVol := corev1.Volume{Name: "s", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: sel.LocalObjectReference}}}}}}
	mount := []corev1.VolumeMount{{Name: "s", MountPath: "/s"}}
	envSecret := []corev1.EnvVar{{Name: "T", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: sel}}}
	side := func(c corev1.Container) wfv1.UserContainer {
		c.Name, c.Image = "side", "example.test/image:1"
		return wfv1.UserContainer{Container: c}
	}
	s3 := wfv1.ArtifactLocation{S3: &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{Endpoint: "objects.example.test", Bucket: "b", AccessKeySecret: sel}, Key: "k"}}
	shapes := map[string]func(w *wfv1.Workflow){
		"01 main env": func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Env = envSecret },
		"02 sidecar envFrom": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Sidecars = []wfv1.UserContainer{side(corev1.Container{EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: sel.LocalObjectReference}}}})}
		},
		"03 init env": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].InitContainers = []wfv1.UserContainer{side(corev1.Container{Env: envSecret})}
		},
		"04 templateDefaults env": func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Env: envSecret}}
		},
		"05 templateDefaults mount of workflow volume": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{secretVol}
			w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{VolumeMounts: mount}}
		},
		"06 workflow projected secret, script main": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{projVol}
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].Script = &wfv1.ScriptTemplate{Container: corev1.Container{Image: "example.test/image:1", Command: []string{"sh"}, VolumeMounts: mount}, Source: "exit 7"}
		},
		"07 workflow secret mounted only by sidecar": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{secretVol}
			w.Spec.Templates[0].Sidecars = []wfv1.UserContainer{side(corev1.Container{VolumeMounts: mount})}
		},
		"08 template secret volume, init mount": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Volumes = []corev1.Volume{secretVol}
			w.Spec.Templates[0].InitContainers = []wfv1.UserContainer{side(corev1.Container{VolumeMounts: mount})}
		},
		"09 script env": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].Script = &wfv1.ScriptTemplate{Container: corev1.Container{Image: "example.test/image:1", Command: []string{"sh"}, Env: envSecret}, Source: "exit 7"}
		},
		"10 input artifact s3": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/in", ArtifactLocation: s3}}
		},
		"11 output artifact s3": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Outputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/out", ArtifactLocation: s3}}
		},
		"12 templateDefaults archive": func(w *wfv1.Workflow) { w.Spec.TemplateDefaults = &wfv1.Template{ArchiveLocation: &s3} },
		"13 steps-called leaf env": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Env = envSecret
			w.Spec.Entrypoint = "outer"
			w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "outer", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "main"}, {Name: "b", Template: "main"}}}}})
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			w := simpleWorkflow()
			shape(w)
			r := requestFor(t, w)
			r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", "cluster-secret", "private")}
			hooks := identityHooks()
			seen := admissioncontract.Plan{}
			hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, in admissioncontract.TransformInput) error {
				seen = in.Plan
				return nil
			}
			p, err := Prepare(t.Context(), r, Policy{}, hooks)
			if err != nil {
				t.Logf("refused at admission (no hold possible): %v", err)
				return
			}
			for _, e := range append(p.View().Plan.Entries, seen.Entries...) {
				if e.HoldEligible {
					t.Fatalf("secret route eligible: %+v", e)
				}
			}
			t.Logf("admitted, ineligible: %q", p.View().Plan.Entries[0].HoldRefusal)
		})
	}
}

// Dormancy: with no assist, the hook still receives defaults merged into every template.
func TestReviewEffectiveInputsRewriteEveryWorkflow(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Env: []corev1.EnvVar{{Name: "FROM_DEFAULTS", Value: "1"}}}}
	hooks := identityHooks()
	merged := false
	hooks.TransformNative = func(_ context.Context, out *wfv1.Workflow, _ admissioncontract.TransformInput) error {
		for _, e := range out.Spec.Templates[0].Container.Env {
			merged = merged || e.Name == "FROM_DEFAULTS"
		}
		return nil
	}
	if _, err := Prepare(t.Context(), requestFor(t, w), Policy{}, hooks); err != nil {
		t.Fatal(err)
	}
	if merged {
		t.Fatal("templateDefaults pre-merged into authored templates for every hooked Workflow")
	}
}

// A workflow-level token reaching a containerSet step through containerSet.volumeMounts.
func TestReviewContainerSetSharedTokenNeedsGrant(t *testing.T) {
	t.Parallel()
	w, fixture := tokenWorkflow()
	w.Spec.Volumes = w.Spec.Templates[0].Volumes
	w.Spec.Templates[0].Volumes = nil
	name := w.Spec.Volumes[0].Name
	w.Spec.Templates[0].Container.Command = []string{"true"}
	w.Spec.Templates[0].Inputs.Artifacts = nil
	w.Spec.Templates[0].Container.WorkingDir = ""
	b := wfv1.Template{Name: "b", ContainerSet: &wfv1.ContainerSetTemplate{VolumeMounts: []corev1.VolumeMount{{Name: name, MountPath: "/tok", ReadOnly: true}}, Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: w.Spec.Templates[0].Container.Image, Command: []string{"true"}}}}}}
	entry := w.Spec.Entrypoint
	w.Spec.Templates = append(w.Spec.Templates, b, wfv1.Template{Name: "outer", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: entry}}}, {Steps: []wfv1.WorkflowStep{{Name: "b", Template: "b"}}}}})
	w.Spec.Entrypoint = "outer"
	r := requestFor(t, w)
	r.Grants = fixture.Grants
	if _, err := Prepare(t.Context(), r, fixture.Policy, identityHooks()); err == nil {
		t.Fatal("containerSet step b mounts the workflow-level identity token with no check")
	} else {
		t.Log("refused:", err)
	}
}
