package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const retiredSyncLimitsName = "oberth-sync-limits"

func retireSyncLimitsBeforeHelm(ctx context.Context, cfg Config, deps Deps, ns string, tracked bool) error {
	if err := orphanRetiredSyncLimits(ctx, cfg, deps, ns); err != nil {
		return err
	}
	if !tracked {
		return nil
	}
	return verifyRetiredSyncLimitsKept(ctx, deps, ns)
}

func isRetiredSyncLimitsObject(object any, namespace string) bool {
	entry, ok := object.(map[string]any)
	if !ok || entry["apiVersion"] != "v1" || entry["kind"] != "ConfigMap" {
		return false
	}
	meta, _ := entry["metadata"].(map[string]any)
	return meta["name"] == retiredSyncLimitsName && (meta["namespace"] == namespace || meta["namespace"] == nil)
}

// Reuse-values would otherwise render the retired object again. Filter this
// object alone and preserve the complete remaining extraObjects list.
func removeSyncLimitsValues(ctx context.Context, deps Deps, ns string, exists bool) ([]string, bool, error) {
	if !exists {
		return nil, false, nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", ns, "-o", "json"})
	if err != nil {
		return nil, false, fmt.Errorf("read Argo values to remove retired semaphore management: %w", err)
	}
	var stored map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&stored); err != nil {
		return nil, false, fmt.Errorf("decode Argo values: %w", err)
	}
	objects, _ := stored["extraObjects"].([]any)
	rest := make([]any, 0, len(objects))
	found := false
	for _, object := range objects {
		if isRetiredSyncLimitsObject(object, ns) {
			found = true
		} else {
			rest = append(rest, object)
		}
	}
	if !found {
		return nil, false, nil
	}
	encoded, _ := json.Marshal(rest)
	return []string{"--set-json", "extraObjects=" + string(encoded)}, true, nil
}

// Helm reads the live keep annotation before deleting resources removed from a
// release. Detach ownership before upgrading; later manifests omit this object.
// Patch metadata alone so concurrent operator changes to semaphore data survive.
func orphanRetiredSyncLimits(ctx context.Context, cfg Config, deps Deps, ns string) error {
	if deps.KubeClient == nil {
		return nil
	}
	cms := deps.KubeClient.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, retiredSyncLimitsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	owner, ownerNS := cm.Annotations["meta.helm.sh/release-name"], cm.Annotations["meta.helm.sh/release-namespace"]
	oberthNS := cfg.Namespace
	if oberthNS == "" {
		oberthNS = "oberth"
	}
	if (owner != "argo-workflows" || ownerNS != ns) && (owner != "oberth" || ownerNS != oberthNS) {
		return nil
	}
	if cm.DeletionTimestamp != nil {
		return fmt.Errorf("cannot orphan deleting semaphore ConfigMap %s/%s", ns, cm.Name)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": cm.UID, "resourceVersion": cm.ResourceVersion,
		"annotations": map[string]any{"meta.helm.sh/release-name": nil, "meta.helm.sh/release-namespace": nil, "helm.sh/resource-policy": "keep"},
		"labels":      map[string]any{"app.kubernetes.io/managed-by": nil},
	}})
	if err != nil {
		return err
	}
	if _, err = cms.Patch(ctx, cm.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("orphan semaphore ConfigMap %s/%s before Helm upgrade: %w", ns, cm.Name, err)
	}
	_, _ = fmt.Fprintf(deps.Output, "Kept ConfigMap %s/%s and its semaphore data; it is now operator-owned.\n", ns, cm.Name)
	return nil
}

// A prior Helm manifest can still track an operator replacement that this
// installer must not modify. Helm refetches it and deletes it unless its live
// keep annotation is set. Refuse rather than adopt or delete that replacement.
func verifyRetiredSyncLimitsKept(ctx context.Context, deps Deps, ns string) error {
	if deps.KubeClient == nil {
		return fmt.Errorf("cannot safely retire semaphore ConfigMap %s/%s without a Kubernetes client", ns, retiredSyncLimitsName)
	}
	cm, err := deps.KubeClient.CoreV1().ConfigMaps(ns).Get(ctx, retiredSyncLimitsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify retained semaphore ConfigMap before Helm upgrade: %w", err)
	}
	if cm.Annotations["helm.sh/resource-policy"] != "keep" {
		return fmt.Errorf("refusing Helm upgrade: the prior release still tracks ConfigMap %s/%s, but its current object is not marked keep; left it untouched; explicitly retain or resolve its release tracking before retrying", ns, cm.Name)
	}
	return nil
}
