package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Upgrade observation uses native Workflow state for every server version.
// Critical selection conservatively waits for all active Oberth Workflows;
// this read is advisory and does not freeze admission or inspect a server queue.
func waitForUpgradeWorkflows(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	if deps.RestConfig == nil {
		return errors.New("Kubernetes Workflow observation unavailable; confirm no runs are active and retry without --wait-idle or --wait-for")
	}
	if err := verifyRecordedCluster(deps.ContextName, deps.RestConfig); err != nil {
		return err
	}
	namespace, err := releasePipelineNamespace(ctx, deps, cfg.Namespace)
	if err != nil {
		return err
	}
	if namespace == "" {
		namespace = DefaultArgoNamespace
	}
	kube, err := dynamic.NewForConfig(deps.RestConfig)
	if err != nil {
		return err
	}
	limit := cfg.WaitTimeout
	if limit == 0 {
		limit = DefaultUpgradeWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	output := deps.Output
	if output == nil {
		output = io.Discard
	}
	var nextProgress time.Time
	for {
		list, err := kube.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"}).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "oberth.ci/run"})
		if err != nil {
			return fmt.Errorf("cannot observe active Kubernetes Workflows; confirm no runs are active before retrying without --wait-idle or --wait-for: %w", err)
		}
		active := 0
		for _, item := range list.Items {
			status, _ := item.Object["status"].(map[string]any)
			switch status["phase"] {
			case "Succeeded", "Failed", "Error":
			default:
				active++
			}
		}
		if active == 0 {
			return nil
		}
		if !time.Now().Before(nextProgress) {
			if _, err := fmt.Fprintf(output, "Waiting for native Kubernetes Workflows: active=%d (all active Workflows, including critical or unknown)\n", active); err != nil {
				return err
			}
			nextProgress = time.Now().Add(15 * time.Second)
		}
		if err := sleepContext(ctx, pollInterval(deps)); err != nil {
			return fmt.Errorf("wait for active Kubernetes Workflows: %w", err)
		}
	}
}
