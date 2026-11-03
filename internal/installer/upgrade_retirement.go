package installer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// The retirement release must not replace the only process that can clean up
// a legacy execution. Keep the running owner available; never infer cleanup
// from a terminal phase, a deletion timestamp, or a missing database row.
func checkUpgradeLegacyResources(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	if semver.Compare(canonicalChartVersion(cfg.BinaryVersion), "v0.17.83-0") < 0 {
		return nil
	}
	namespace, err := installedPipelineNamespace(ctx, cfg, deps)
	if err != nil {
		return fmt.Errorf("inspect legacy executions before upgrade: %w", err)
	}
	if deps.RestConfig == nil {
		return errors.New("inspect legacy executions before upgrade: Kubernetes connection is required")
	}
	config := rest.CopyConfig(deps.RestConfig)
	config.Timeout = 10 * time.Second
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("inspect legacy executions before upgrade: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("inspect legacy executions before upgrade: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, group := range []struct {
		version, namespace string
		resources          []string
	}{
		{"argoproj.io/v1alpha1", namespace, []string{"workflows"}},
		// The historical VM backend accepted its own namespace. Inspect all
		// namespaces, but block only objects carrying Oberth legacy ownership.
		{"kubevirt.io/v1", metav1.NamespaceAll, []string{"virtualmachineinstances", "virtualmachines"}},
	} {
		advertised, err := discoveryClient.ServerResourcesForGroupVersion(group.version)
		if apierrors.IsNotFound(err) {
			continue
		} // CRDs absent, not an unreadable inventory.
		if err != nil {
			return fmt.Errorf("inspect legacy %s API before upgrade: %w", group.version, err)
		}
		gv, err := schema.ParseGroupVersion(group.version)
		if err != nil {
			return err
		}
		for _, resource := range group.resources {
			available := false
			for _, api := range advertised.APIResources {
				if api.Name == resource && api.Namespaced && slices.Contains(api.Verbs, "list") {
					available = true
				}
			}
			if !available {
				return fmt.Errorf("inspect legacy %s before upgrade: API discovery omitted a listable namespaced resource", resource)
			}
			inventory := client.Resource(gv.WithResource(resource)).Namespace(group.namespace)
			cursor := ""
			seen := map[string]bool{}
			for page := 0; ; page++ {
				if page >= 100 {
					return fmt.Errorf("inspect legacy %s before upgrade: inventory exceeded 100 pages; narrow the cluster inventory before retrying", resource)
				}
				objects, err := inventory.List(ctx, metav1.ListOptions{Limit: 500, Continue: cursor})
				if err != nil {
					return fmt.Errorf("inspect legacy %s before upgrade: %w", resource, err)
				}
				var names []string
				for _, object := range objects.Items {
					if legacyUpgradeResource(resource, &object) {
						names = append(names, fmt.Sprintf("%s %s/%s", resource, object.GetNamespace(), object.GetName()))
						if len(names) == 20 {
							break
						}
					}
				}
				if len(names) != 0 {
					slices.Sort(names)
					retention := ""
					if resource == "workflows" {
						retention = " Completed preflight Workflows also count: wait for argo.workflowTTL collection (default 1 hour) or deletion by the current server."
					}
					return fmt.Errorf("upgrade refused: legacy executions still exist: %s; keep the current server running, stop new submissions, finish or cancel these runs and wait for confirmed resource deletion before retrying.%s See docs/retirement-upgrade.md", strings.Join(names, ", "), retention)
				}
				cursor = objects.GetContinue()
				if cursor == "" {
					break
				}
				if seen[cursor] {
					return fmt.Errorf("inspect legacy %s before upgrade: repeated inventory cursor", resource)
				}
				seen[cursor] = true
			}
		}
	}
	return nil
}

func legacyUpgradeResource(resource string, object *unstructured.Unstructured) bool {
	annotations, labels := object.GetAnnotations(), object.GetLabels()
	if resource == "workflows" {
		_, trusted := annotations["oberth.ci/trusted-kvm-intent"]
		return trusted
	}
	_, spec := annotations["oberth.ci/vm-spec"]
	_, identity := annotations["oberth.ci/vm-identity"]
	return labels["oberth.ci/tier"] == "vm-runner" || spec || identity
}
