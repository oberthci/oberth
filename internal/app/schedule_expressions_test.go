package app_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestOrdinaryPushKeepsSpecExpressions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, field, value string }{
		{"pod-name", "pod", "{{workflow.name}}"},
		{"workflow-name", "workflow", "{{workflow.name}}"},
		{"argument-name", "argument", "{{workflow.name}}"},
		{"pod-namespace", "pod", "{{workflow.namespace}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := workflow()
			switch tc.field {
			case "pod":
				w.Spec.PodMetadata = &wf.Metadata{Annotations: map[string]string{"run-note": tc.value}}
			case "workflow":
				w.Spec.WorkflowMetadata = &wf.WorkflowMetadata{Annotations: map[string]string{"run-note": tc.value}}
			case "argument":
				w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "x", Value: wf.AnyStringPtr(tc.value)}}
			}
			submitted := recordOrdinaryPush(t, w, "refs/heads/topic")
			if !reflect.DeepEqual(w.Spec.PodMetadata, submitted.Spec.PodMetadata) || !reflect.DeepEqual(w.Spec.WorkflowMetadata, submitted.Spec.WorkflowMetadata) {
				t.Fatal("ordinary submission changed authored metadata")
			}
			if tc.field == "argument" && !reflect.DeepEqual(w.Spec.Arguments.Parameters[0], submitted.Spec.Arguments.Parameters[0]) {
				t.Fatal("ordinary submission changed the authored argument")
			}
		})
	}
}

func TestScheduleChecksFinalTransformedSpec(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.native.Hook.TransformNative = func(_ context.Context, w *wf.Workflow, _ admissioncontract.TransformInput) error {
		w.Spec.PodMetadata = &wf.Metadata{Annotations: map[string]string{"from-hook": "{{workflow.name}}"}}
		return nil
	}
	r := request()
	r.ScheduleEntry = "nightly"
	p, err := f.new(t).PrepareSchedule(t.Context(), r)
	if err == nil || p != nil || !strings.Contains(err.Error(), "spec.podMetadata.annotations.from-hook") {
		t.Fatal("schedule guard ran before the final native transform")
	}
}
