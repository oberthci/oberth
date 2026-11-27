package installer_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
)

// sourceBudgetTuple is the chart wire shape, independent of the server implementation.
type sourceBudgetTuple struct {
	Exports         int `json:"exports"`
	Archives        int `json:"archives"`
	Transfers       int `json:"transfers"`
	Auth            int `json:"auth"`
	WorkflowWaiters int `json:"workflow_waiters"`
}

func TestChartSourceTupleAndServerResourceEnvelope(t *testing.T) {
	for _, b := range []sourceBudgetTuple{sourceBudgetTuple{Exports: 1, Archives: 2, Transfers: 2, Auth: 32, WorkflowWaiters: 16}, {Exports: 2, Archives: 4, Transfers: 4, Auth: 32, WorkflowWaiters: 16}, {Exports: 4, Archives: 8, Transfers: 8, Auth: 128, WorkflowWaiters: 16}} {
		values := map[string]any{"server": map[string]any{"runtime": map[string]any{"source_delivery": sourceTupleValues(b)}}, "resources": map[string]any{"requests": map[string]any{"cpu": "250m", "memory": "1Gi"}, "limits": map[string]any{"cpu": "2", "memory": "3Gi"}}}
		objects := rendered(t, values)
		cm := object(t, objects, "ConfigMap", "oberth-server")["data"].(map[string]any)
		var config struct {
			Runtime struct {
				SourceDelivery sourceBudgetTuple `json:"source_delivery"`
			} `json:"runtime"`
		}
		if err := json.Unmarshal([]byte(cm["server.json"].(string)), &config); err != nil {
			t.Fatal(err)
		}
		if config.Runtime.SourceDelivery != b {
			t.Fatal("chart lost runtime tuple", config.Runtime.SourceDelivery, b)
		}
		var deployment appsv1.Deployment
		decodeObject(t, object(t, objects, "Deployment", "oberth"), &deployment)
		container := deployment.Spec.Template.Spec.Containers[0]
		if container.Name != "server" || container.Resources.Requests.Cpu().String() != "250m" || container.Resources.Requests.Memory().String() != "1Gi" || container.Resources.Limits.Cpu().String() != "2" || container.Resources.Limits.Memory().String() != "3Gi" {
			t.Fatal("server envelope missing", container.Resources)
		}
	}
	var baseline appsv1.Deployment
	decodeObject(t, object(t, rendered(t, nil), "Deployment", "oberth"), &baseline)
	if len(baseline.Spec.Template.Spec.Containers[0].Resources.Limits) != 0 {
		t.Fatal("qualification limits changed default allocation")
	}
}

func TestChartRejectsInvalidSourceTuples(t *testing.T) {
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []sourceBudgetTuple{{}, {Exports: 9, Archives: 16, Transfers: 16, Auth: 32, WorkflowWaiters: 16}, {Exports: 2, Archives: 1, Transfers: 2, Auth: 32, WorkflowWaiters: 16}, {Exports: 2, Archives: 2, Transfers: 1, Auth: 32, WorkflowWaiters: 16}} {
		_, err := installer.Render(ch, map[string]any{"server": map[string]any{"runtime": map[string]any{"source_delivery": sourceTupleValues(b)}}}, "oberth")
		if err == nil {
			t.Fatal("chart accepted invalid tuple", b)
		}
	}
}

func sourceTupleValues(b sourceBudgetTuple) map[string]any {
	return map[string]any{"exports": b.Exports, "archives": b.Archives, "transfers": b.Transfers, "auth": b.Auth, "workflow_waiters": b.WorkflowWaiters}
}
