package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"strings"

	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Namespace use is recorded in native release storage, even for unmarked namespaces.
func (e *Engine) checkNamespaceReleases(ctx context.Context, c Choices) error {
	secrets, err := e.Cluster.Client.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		return problem("cluster", err)
	}
	if len(secrets.Items) == 0 {
		return nil
	}
	records, err := namespaceReleaseRecords(secrets)
	if err != nil {
		return err
	}

	return heldNamespace(c, records)
}

func heldNamespace(c Choices, records []*release.Release) error {
	for _, record := range currentNamespaceRevisions(records) {
		names, err := releaseNamespaces(record)
		if err != nil {
			return described("release-membership", err, fmt.Sprintf("Helm release: %s/%s; revision: %d; %v", record.Namespace, record.Name, record.Version, err))
		}
		if record.Namespace == c.Namespace && record.Name == "oberth" {
			if product, _ := oberthRelease(record); product {
				continue
			}
		}
		for _, name := range []string{c.Namespace, c.PipelineNamespace} {
			if names[name] {
				return namespaceOwner(name, record.Namespace, record.Name)
			}
		}
	}
	return nil
}

func releaseNamespaces(record *release.Release) (map[string]bool, error) {
	product, err := oberthRelease(record)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	if product {
		if err := productNamespaces(record, names); err != nil {
			return nil, err
		}
	}
	if err := manifestNamespaces(record.Manifest, names, product); err != nil {
		return nil, err
	}
	for _, hook := range record.Hooks {
		if hook == nil {
			return nil, fmt.Errorf("release hook is unreadable")
		}
		if err := manifestNamespaces(hook.Manifest, names, product); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func manifestNamespaces(manifest string, names map[string]bool, product bool) error {
	decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		object := &unstructured.Unstructured{}
		err := decoder.Decode(object)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(object.Object) == 0 {
			continue
		}
		if err := manifestObjectNamespaces(object, names, product); err != nil {
			return err
		}
	}
}

// Decode the single cluster-wide snapshot through Helm's native decoder.
type namespaceReleaseSnapshot struct {
	typedcorev1.SecretInterface
	items *corev1.SecretList
}

func (s namespaceReleaseSnapshot) List(context.Context, metav1.ListOptions) (*corev1.SecretList, error) {
	return s.items, nil
}

func pipelineNamespaceCreation(p *Plan, ns *corev1.Namespace) bool {
	if ns == nil {
		p.podSecurity = "baseline (new namespace)"
	} else {
		p.podSecurity = ns.Labels[podSecurityEnforce]
	}
	for _, revision := range []Revision{p.history.Latest, p.history.Deployed} {
		pipeline, _ := revision.Values["pipeline"].(map[string]any)
		if create, ok := pipeline["createNamespace"].(bool); ok {
			return create
		}
	}
	return ns == nil || ownedByRelease(ns, p.choices.Namespace)
}

func manifestObjectNamespaces(object *unstructured.Unstructured, names map[string]bool, product bool) error {
	if object.IsList() {
		return object.EachListItem(func(item runtime.Object) error {
			child, ok := item.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("release manifest membership is unresolved")
			}
			return manifestObjectNamespaces(child, names, product)
		})
	}
	if object.GetKind() == "" {
		return fmt.Errorf("release manifest membership is unresolved")
	}
	if !product && object.GetKind() != "Namespace" {
		return nil
	}
	namespace, _, err := unstructured.NestedString(object.Object, "metadata", "namespace")
	if err != nil {
		return err
	}
	if object.GetKind() == "Namespace" {
		namespace, _, err = unstructured.NestedString(object.Object, "metadata", "name")
		if err == nil && namespace == "" {
			return fmt.Errorf("release manifest membership is unresolved")
		}
	}
	if err != nil {
		return err
	}
	if namespace != "" {
		names[namespace] = true
	}
	return nil
}

func namespaceReleaseRecords(secrets *corev1.SecretList) ([]*release.Release, error) {
	groups := map[string]*corev1.SecretList{}
	for _, secret := range secrets.Items {
		if groups[secret.Namespace] == nil {
			groups[secret.Namespace] = &corev1.SecretList{}
		}
		groups[secret.Namespace].Items = append(groups[secret.Namespace].Items, secret)
	}
	var all []*release.Release
	for namespace, items := range groups {
		records, err := newReleaseSecrets(namespaceReleaseSnapshot{items: items}).Query(map[string]string{"owner": "helm"})
		if err != nil {
			return nil, described("release-membership", err, "Release namespace: "+namespace)
		}
		for _, record := range records {
			if record.Namespace != namespace {
				return nil, described("release-membership", nil, "Release namespace: "+namespace)
			}
			all = append(all, record)
		}
	}
	return all, nil
}

func oberthRelease(record *release.Release) (bool, error) {
	if record.Name == "" || record.Namespace == "" || record.Version < 1 {
		return false, fmt.Errorf("release identity is incomplete")
	}
	if record.Chart != nil && record.Chart.Metadata != nil && record.Chart.Name() != "" {
		return record.Chart.Name() == "oberth", nil
	}
	if record.Name == "oberth" {
		return true, nil
	}
	return false, fmt.Errorf("release chart identity is unreadable")
}

func productNamespaces(record *release.Release, names map[string]bool) error {
	names[record.Namespace] = true
	pipeline, found, err := unstructured.NestedString(record.Config, "pipeline", "namespace")
	if err != nil || found && pipeline == "" {
		return fmt.Errorf("recorded pipeline namespace is unreadable")
	}
	if found {
		names[pipeline] = true
	}
	return nil
}

// Failed or pending changes can coexist with the last deployed resources.
// Superseded history cannot add a third live manifest to that pair.
func currentNamespaceRevisions(records []*release.Release) []*release.Release {
	type revisions struct{ latest, deployed *release.Release }
	groups := map[string]revisions{}
	for _, record := range records {
		key := record.Namespace + "/" + record.Name
		pair := groups[key]
		if pair.latest == nil || record.Version > pair.latest.Version {
			pair.latest = record
		}
		if record.Info != nil && record.Info.Status == release.StatusDeployed && (pair.deployed == nil || record.Version > pair.deployed.Version) {
			pair.deployed = record
		}
		groups[key] = pair
	}
	selected := make([]*release.Release, 0, 2*len(groups))
	for _, pair := range groups {
		selected = append(selected, pair.latest)
		if pair.deployed != nil && pair.deployed != pair.latest {
			selected = append(selected, pair.deployed)
		}
	}
	return selected
}
