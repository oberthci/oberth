package installer

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func (e *Engine) activeRuns(ctx context.Context, namespace string) (int, error) {
	client, err := dynamic.NewForConfig(e.Cluster.Config)
	if err != nil {
		return 0, problem("cluster", err)
	}
	workflows := client.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"}).Namespace(namespace)
	count, next := 0, ""
	for {
		page, err := workflows.List(ctx, metav1.ListOptions{Limit: 200, Continue: next})
		if err != nil {
			return 0, problem("cluster", err)
		}
		for _, workflow := range page.Items {
			status, _ := workflow.Object["status"].(map[string]any)
			phase, _ := status["phase"].(string)
			if phase != "Succeeded" && phase != "Failed" && phase != "Error" {
				count++
			}
		}
		next = page.GetContinue()
		if next == "" {
			return count, nil
		}
	}
}
