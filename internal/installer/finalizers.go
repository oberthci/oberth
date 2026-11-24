package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	argoclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// FinalizerScope binds destructive installation removal to its pipeline
// namespace and controller instance. Other installations and finalizers stay.
type FinalizerScope struct{ Namespace, Instance string }

const archiveFinalizer = "oberth.ci/run-record"

func (e *Engine) releaseRunFinalizers(ctx context.Context, scope FinalizerScope) error {
	if e.Finalizers != nil {
		return e.Finalizers(ctx, scope)
	}
	if e.Cluster.Config == nil {
		return problem("wiring", nil)
	}
	client, err := argoclient.NewForConfig(e.Cluster.Config)
	if err != nil {
		return err
	}
	return ReleaseRunFinalizers(ctx, client, scope)
}

// ReleaseRunFinalizers is used before rollback and by the pre-delete Helm hook.
// It grants no Workflow deletion authority and never removes another finalizer.
func ReleaseRunFinalizers(ctx context.Context, client argoclient.Interface, scope FinalizerScope) error {
	if scope.Namespace == "" || scope.Instance == "" {
		return fmt.Errorf("installation finalizer scope is missing")
	}
	if err := releaseCronFinalizers(ctx, client, scope); err != nil {
		return err
	}
	workflows := client.ArgoprojV1alpha1().Workflows(scope.Namespace)
	options := meta.ListOptions{LabelSelector: "oberth.ci/engine=argo,workflows.argoproj.io/controller-instanceid=" + scope.Instance, Limit: 200}
	for {
		page, err := workflows.List(ctx, options)
		if err != nil {
			return err
		}
		for _, w := range page.Items {
			if !scope.matches(w.Namespace, w.Labels) || w.Labels["oberth.ci/engine"] != "argo" || !slices.Contains(w.Finalizers, archiveFinalizer) {
				continue
			}
			finalizers := slices.DeleteFunc(slices.Clone(w.Finalizers), func(f string) bool { return f == archiveFinalizer })
			patch, _ := json.Marshal([]map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": w.UID},
				{"op": "test", "path": "/metadata/resourceVersion", "value": w.ResourceVersion},
				{"op": "replace", "path": "/metadata/finalizers", "value": finalizers},
			})
			if _, err = workflows.Patch(ctx, w.Name, types.JSONPatchType, patch, meta.PatchOptions{}); err != nil {
				return err
			}
		}
		if page.Continue == "" {
			return nil
		}
		options.Continue = page.Continue
	}
}

func (scope FinalizerScope) matches(namespace string, labels map[string]string) bool {
	return namespace == scope.Namespace && labels["workflows.argoproj.io/controller-instanceid"] == scope.Instance
}

// Clear inheritance first, so an older server cannot leave future scheduled
// children with a finalizer that it does not know how to release.
func releaseCronFinalizers(ctx context.Context, client argoclient.Interface, scope FinalizerScope) error {
	parents := client.ArgoprojV1alpha1().CronWorkflows(scope.Namespace)
	options := meta.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=oberth,workflows.argoproj.io/controller-instanceid=" + scope.Instance, Limit: 200}
	for {
		page, err := parents.List(ctx, options)
		if err != nil {
			return err
		}
		for _, parent := range page.Items {
			if parent.Namespace != scope.Namespace || parent.Labels["app.kubernetes.io/managed-by"] != "oberth" || parent.Labels["workflows.argoproj.io/controller-instanceid"] != scope.Instance || parent.Spec.WorkflowMetadata == nil || !slices.Contains(parent.Spec.WorkflowMetadata.Finalizers, archiveFinalizer) {
				continue
			}
			parent.Spec.WorkflowMetadata.Finalizers = slices.DeleteFunc(slices.Clone(parent.Spec.WorkflowMetadata.Finalizers), func(f string) bool { return f == archiveFinalizer })
			if _, err = parents.Update(ctx, &parent, meta.UpdateOptions{}); err != nil {
				return err
			}
		}
		if page.Continue == "" {
			return nil
		}
		options.Continue = page.Continue
	}
}
