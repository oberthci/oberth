package argoworkflow

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestHoldNamespaceCollisionOnlyChangesEligibility(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "init", "sidecar", "defaults", "inline"} {
		for _, shape := range []string{"env", "mount", "device"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				checkHoldNamespaceCollision(t, kind, shape)
			})
		}
	}
}
func collideHoldContainer(t *wf.Template, shape string) {
	for _, c := range containers(t) {
		c.container.VolumeMounts = nil
		if shape == "env" {
			c.container.Env = []core.EnvVar{{Name: "OBERTH_HOLD_TTL", Value: "10"}}
		} else {
			c.container.VolumeDevices = []core.VolumeDevice{{Name: "data", DevicePath: "/oberth-hold/state"}}
		}
	}
	for i := range t.Steps {
		for j := range t.Steps[i].Steps {
			if child := t.Steps[i].Steps[j].Inline; child != nil {
				collideHoldContainer(child, shape)
			}
		}
	}
}

func checkHoldNamespaceCollision(t *testing.T, kind, shape string) {
	t.Helper()
	w, p := mountDocument(kind, "/oberth-hold/work")
	if shape != "mount" {
		for i := range w.Spec.Templates {
			collideHoldContainer(&w.Spec.Templates[i], shape)
		}
		if w.Spec.TemplateDefaults != nil {
			collideHoldContainer(w.Spec.TemplateDefaults, shape)
		}
	}
	prepared, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
	if err != nil {
		t.Fatal("collision changed ordinary admission", err)
	}
	for _, entry := range prepared.View().Plan.Entries {
		if entry.HoldEligible || entry.HoldArmed || entry.HoldRefusal != "uses helper namespace" {
			t.Fatal("collision remained eligible", entry)
		}
	}
}
