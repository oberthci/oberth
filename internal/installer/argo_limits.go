package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Run limits live in Argo (docs/where-limits-live.md, #1517). The installer
// writes them into the argo-workflows release it manages:
//
//   - controller.parallelism and controller.namespaceParallelism are the
//     controller's own workflow limits. Unset (the default) is no limit; 0 is
//     no limit too. The controller is single-namespace, so either bounds every
//     pipeline Workflow.
//   - the ConfigMap oberth-sync-limits, rendered through the chart's
//     extraObjects in the pipeline namespace, carries the semaphore limits
//     repository steps reference with synchronization.semaphores
//     configMapKeyRef. Name and namespace are the ones v0.18 and v0.19
//     rendered from the Oberth chart, so no repository changes.
//
// Nothing here changes unless the operator passes the corresponding flag;
// --reuse-values keeps whatever an earlier install set.

// SyncLimitsConfigMap is the fixed name repository workflows reference.
const SyncLimitsConfigMap = "oberth-sync-limits"

const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	helmResourcePolicyAnnotation   = "helm.sh/resource-policy"
	helmManagedByLabel             = "app.kubernetes.io/managed-by"
	argoReleaseName                = "argo-workflows"
	maxArgoParallelism             = 100000
)

var syncLimitKey = regexp.MustCompile(`^[A-Za-z0-9]([-._A-Za-z0-9]{0,251}[A-Za-z0-9])?$`)

// ParseArgoSyncLimit parses one --argo-sync-limit key=N.
func ParseArgoSyncLimit(value string) (string, int, error) {
	key, raw, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || !syncLimitKey.MatchString(key) {
		return "", 0, fmt.Errorf("--argo-sync-limit %q: want key=N with a ConfigMap key (letters, digits, '-', '_', '.')", value)
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 999999 {
		return "", 0, fmt.Errorf("--argo-sync-limit %q: the limit must be a positive integer", value)
	}
	return key, limit, nil
}

// validateArgoLimits checks the limit flags before anything is installed.
func validateArgoLimits(cfg Config) error {
	for name, value := range map[string]*int{"--argo-parallelism": cfg.ArgoParallelism, "--argo-namespace-parallelism": cfg.ArgoNamespaceParallelism} {
		if value != nil && (*value < 0 || *value > maxArgoParallelism) {
			return fmt.Errorf("%s must be 0 (no limit) to %d", name, maxArgoParallelism)
		}
	}
	if (cfg.ArgoParallelism != nil || cfg.ArgoNamespaceParallelism != nil || cfg.ArgoSyncLimits != nil) && cfg.SkipArgo {
		return errors.New("--argo-parallelism, --argo-namespace-parallelism and --argo-sync-limit configure the managed argo-workflows release; set them on your own controller when --skip-argo-workflows is used")
	}
	for key, limit := range cfg.ArgoSyncLimits {
		if !syncLimitKey.MatchString(key) || limit < 1 {
			return fmt.Errorf("argo sync limit %q=%d is invalid", key, limit)
		}
	}
	return nil
}

// argoLimitsRequested reports whether this run changes any Argo limit, which
// forces a helm upgrade of an otherwise current argo-workflows release.
func argoLimitsRequested(cfg Config) bool {
	return cfg.ArgoParallelism != nil || cfg.ArgoNamespaceParallelism != nil || cfg.ArgoSyncLimits != nil
}

// argoLimitArgs renders the requested limits as helm arguments for the
// argo-workflows chart. extraObjects is one list and --set wins over
// --values, so while the nonroot controller profile is active its immutable
// controller ConfigMap goes into the same list: a list without it would make
// Helm delete the controller's configuration under the running controller.
func argoLimitArgs(cfg Config, namespace string) []string {
	var args []string
	if cfg.ArgoParallelism != nil {
		args = append(args, "--set", fmt.Sprintf("controller.parallelism=%d", *cfg.ArgoParallelism))
	}
	if cfg.ArgoNamespaceParallelism != nil {
		args = append(args, "--set", fmt.Sprintf("controller.namespaceParallelism=%d", *cfg.ArgoNamespaceParallelism))
	}
	if cfg.ArgoSyncLimits != nil {
		objects := []any{}
		if cfg.ArgoControllerProfile != "" {
			objects = append(objects, nonrootProfileObjects(namespace, cfg.nonrootProfile())...)
		}
		if object := syncLimitsConfigMap(cfg.ArgoSyncLimits, namespace); object != nil {
			objects = append(objects, object)
		}
		// Marshaling strings and string maps cannot fail.
		encoded, _ := json.Marshal(objects)
		args = append(args, "--set-json", "extraObjects="+string(encoded))
	}
	return args
}

// syncLimitsConfigMap is the limits ConfigMap as an extraObjects entry, or
// nil for an empty set, which renders no ConfigMap. The installer owns
// extraObjects of the release it manages.
func syncLimitsConfigMap(limits map[string]int, namespace string) map[string]any {
	if len(limits) == 0 {
		return nil
	}
	keys := make([]string, 0, len(limits))
	for key := range limits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	data := map[string]any{}
	for _, key := range keys {
		data[key] = strconv.Itoa(limits[key])
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": SyncLimitsConfigMap, "namespace": namespace,
			"labels": map[string]any{"app.kubernetes.io/part-of": "oberth", "app.kubernetes.io/component": "sync-limits"}},
		"data": data,
	}
}

// isSyncLimitsConfigMap reports an extraObjects entry, as helm get values
// returns it, that is the limits ConfigMap this installer wrote: the fixed
// name in the pipeline namespace with only positive integer limits as data.
func isSyncLimitsConfigMap(object any, namespace string) bool {
	entry, ok := object.(map[string]any)
	if !ok || entry["apiVersion"] != "v1" || entry["kind"] != "ConfigMap" {
		return false
	}
	metadata, ok := entry["metadata"].(map[string]any)
	if !ok || metadata["name"] != SyncLimitsConfigMap || metadata["namespace"] != namespace {
		return false
	}
	data, ok := entry["data"].(map[string]any)
	if !ok || len(data) == 0 {
		return false
	}
	for key, value := range data {
		limit, isString := value.(string)
		if !syncLimitKey.MatchString(key) || !isString {
			return false
		}
		if _, _, err := ParseArgoSyncLimit(key + "=" + limit); err != nil {
			return false
		}
	}
	return true
}

// storedSyncLimitsConfigMap returns the limits ConfigMap an earlier run wrote
// into the release's extraObjects, or nil.
func storedSyncLimitsConfigMap(values map[string]any, namespace string) any {
	objects, _ := values["extraObjects"].([]any)
	for _, object := range objects {
		if isSyncLimitsConfigMap(object, namespace) {
			return object
		}
	}
	return nil
}

// handOverSyncLimits lets the argo-workflows release adopt the limits
// ConfigMap an Oberth v0.18 or v0.19 release rendered. It marks the live
// object for Helm adoption by argo-workflows and with resource-policy keep,
// so the Oberth release's next upgrade, which no longer renders it, leaves
// it in place instead of deleting it under running workflows.
func handOverSyncLimits(ctx context.Context, cfg Config, deps Deps, namespace string) error {
	if cfg.ArgoSyncLimits == nil || deps.KubeClient == nil {
		return nil
	}
	configMaps := deps.KubeClient.CoreV1().ConfigMaps(namespace)
	current, err := configMaps.Get(ctx, SyncLimitsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ConfigMap %s/%s: %w", namespace, SyncLimitsConfigMap, err)
	}
	if current.Annotations[helmReleaseNameAnnotation] == argoReleaseName && current.Annotations[helmReleaseNamespaceAnnotation] == namespace {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{
			helmReleaseNameAnnotation: argoReleaseName, helmReleaseNamespaceAnnotation: namespace, helmResourcePolicyAnnotation: "keep",
		},
		"labels": map[string]string{helmManagedByLabel: "Helm"},
	}})
	if err != nil {
		return err
	}
	if _, err := configMaps.Patch(ctx, SyncLimitsConfigMap, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("hand ConfigMap %s/%s over to the argo-workflows release: %w", namespace, SyncLimitsConfigMap, err)
	}
	_, _ = fmt.Fprintf(deps.Output, "Handed ConfigMap %s/%s over to the argo-workflows release (was %q).\n",
		namespace, SyncLimitsConfigMap, current.Annotations[helmReleaseNameAnnotation])
	return nil
}
