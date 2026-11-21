//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	nativeTemplate "github.com/argoproj/argo-workflows/v4/util/template"
	"github.com/oberthci/oberth/internal/nativebinding"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The pinned public substitution matches the captured controller initialization
// contract; its private reconcile/persist loop is a separate live walk.
func initializedScheduledObservation(t *testing.T, child *wf.Workflow, started time.Time) *wf.Workflow {
	t.Helper()
	observed := child.DeepCopy()
	templates := observed.Spec.Templates
	observed.Spec.Templates = nil
	raw, err := json.Marshal(observed.Spec)
	if err != nil {
		t.Fatal(err)
	}
	rawSpec, err := nativeTemplate.Replace(t.Context(), string(raw), map[string]any{"workflow.name": child.Name}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(rawSpec), &observed.Spec); err != nil {
		t.Fatal(err)
	}
	observed.Spec.Templates = templates
	metadata := observed.Spec.WorkflowMetadata
	if metadata == nil || metadata.LabelsFrom[nativebinding.RunLabel].Expression != "workflow.name" {
		t.Fatal("scheduled identity projection differs")
	}
	maps.Copy(observed.Labels, metadata.Labels)
	maps.Copy(observed.Annotations, metadata.Annotations)
	observed.Labels[nativebinding.RunLabel] = observed.Name
	observed.Status.Phase = wf.WorkflowRunning
	observed.Status.StartedAt = meta.NewTime(started)
	return observed
}
