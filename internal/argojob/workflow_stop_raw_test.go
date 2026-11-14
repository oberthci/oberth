package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type rawStopWorkflowClient struct {
	staticWorkflowClient
	stored []byte
	patch  []byte
}

func (c *rawStopWorkflowClient) Patch(_ context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	if kind != types.JSONPatchType {
		return nil, fmt.Errorf("expected JSON Patch, got %s", kind)
	}
	c.patch = append([]byte(nil), data...)
	patch, err := jsonpatch.DecodePatch(data)
	if err != nil {
		return nil, err
	}
	updated, err := patch.Apply(c.stored)
	if err != nil {
		return nil, err
	}
	var workflow wfv1.Workflow
	if err := json.Unmarshal(updated, &workflow); err != nil {
		return nil, err
	}
	c.stored = updated
	return &workflow, nil
}

func TestStopPatchAppliesToStoredWorkflowMetadataNulls(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", UID: "original"}, Spec: wfv1.WorkflowSpec{VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "work"}}}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
	encoded, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"absent-shutdown", "concurrent-Terminate", "concurrent-Stop", "replaced-UID"} {
		t.Run(mode, func(t *testing.T) {
			var stored map[string]any
			if err := json.Unmarshal(encoded, &stored); err != nil {
				t.Fatal(err)
			}
			spec := stored["spec"].(map[string]any)
			claim := spec["volumeClaimTemplates"].([]any)[0].(map[string]any)
			// Stored by Argo's metadata encoder; Oberth's typed round trip omits it.
			claim["metadata"].(map[string]any)["creationTimestamp"] = nil
			if mode == "concurrent-Terminate" {
				spec["shutdown"] = "Terminate"
			}
			if mode == "concurrent-Stop" {
				spec["shutdown"] = "Stop"
			}
			if mode == "replaced-UID" {
				stored["metadata"].(map[string]any)["uid"] = "replacement"
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			client := &rawStopWorkflowClient{stored: raw}
			result, err := requestWorkflowShutdown(t.Context(), client, workflow, wfv1.ShutdownStrategyStop)
			if mode != "absent-shutdown" {
				if !apierrors.IsConflict(err) {
					t.Fatalf("stale Stop accepted: result=%+v err=%v patch=%s", result, err, client.patch)
				}
				if string(client.stored) != string(raw) {
					t.Fatal("failed comparison mutated the stored Workflow")
				}
				return
			}
			if err != nil || result.Spec.Shutdown != wfv1.ShutdownStrategyStop {
				t.Fatalf("Stop did not apply to stored metadata null: result=%+v err=%v patch=%s", result, err, client.patch)
			}
			if err := json.Unmarshal(client.stored, &stored); err != nil {
				t.Fatal(err)
			}
			claim = stored["spec"].(map[string]any)["volumeClaimTemplates"].([]any)[0].(map[string]any)
			if value, exists := claim["metadata"].(map[string]any)["creationTimestamp"]; !exists || value != nil {
				t.Fatal("Stop rewrote stored claim metadata")
			}
		})
	}
}
