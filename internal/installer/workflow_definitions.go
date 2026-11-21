package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// DefinitionReader supplies only reads of cluster-wide workflow definitions.
type DefinitionReader interface {
	Get(context.Context, string, metav1.GetOptions) (*definitions.CustomResourceDefinition, error)
}

type definitionReview struct {
	Shared []string
	Owners []string
	Digest string
}

func (e *Engine) workflowDefinitions(ctx context.Context, ch *chart.Chart, namespace string, history History) (definitionReview, error) {
	var result definitionReview
	pinned, err := pinnedDefinitions(ch)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	var missing []string
	for _, want := range pinned {
		live, err := e.Definitions.Get(ctx, want.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			missing = append(missing, want.Name)
			continue
		}
		if err != nil {
			return result, described("cluster", err, "Definition: "+want.Name)
		}
		raw, _ := json.Marshal([]any{live.Name, live.UID, live.Spec, live.Status.StoredVersions, live.Labels, live.Annotations})
		_, _ = hash.Write(raw)
		if ownedByRelease(live, namespace) {
			continue
		}
		if detail := compatibleDefinition(want, *live); detail != "" {
			return result, described("definition-schema", nil, "Definition: "+want.Name+"; "+detail)
		}
		result.Shared = append(result.Shared, live.Name)
		owner := live.Annotations["meta.helm.sh/release-name"]
		if owner == "" {
			owner = "unmanaged"
		} else {
			owner = live.Annotations["meta.helm.sh/release-namespace"] + "/" + owner
		}
		if !slices.Contains(result.Owners, owner) {
			result.Owners = append(result.Owners, owner)
		}
	}
	if foreign := missingBorrowedDefinitions(missing, result, history); len(foreign) > 0 {
		return result, described("definition-partial", nil, "Missing definitions: "+strings.Join(foreign, ", ")+"; owners: "+strings.Join(result.Owners, ", "))
	}
	slices.Sort(result.Owners)
	result.Digest = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func pinnedDefinitions(ch *chart.Chart) ([]definitions.CustomResourceDefinition, error) {
	var result []definitions.CustomResourceDefinition
	for _, dependency := range ch.Dependencies() {
		if dependency.Name() != "argoNative" && dependency.Name() != "argo-workflows" {
			continue
		}
		for _, file := range dependency.Files {
			if !strings.HasPrefix(file.Name, "files/crds/minimal/") || !strings.HasSuffix(file.Name, ".yaml") {
				continue
			}
			var crd definitions.CustomResourceDefinition
			if err := yaml.Unmarshal(file.Data, &crd); err != nil {
				return nil, problem("chart-files", err)
			}
			result = append(result, crd)
		}
	}
	if len(result) != 8 {
		return nil, described("chart-files", nil, "Expected eight workflow definitions in the pinned chart.")
	}
	slices.SortFunc(result, func(a, b definitions.CustomResourceDefinition) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

func skipSharedDefinitions(ch *chart.Chart, values map[string]any) {
	shared, _ := values["sharedWorkflowDefinitions"].([]any)
	for _, dependency := range ch.Dependencies() {
		if dependency.Name() != "argoNative" && dependency.Name() != "argo-workflows" {
			continue
		}
		dependency.Files = slices.DeleteFunc(dependency.Files, func(file *chart.File) bool {
			for _, name := range shared {
				name, ok := name.(string)
				if ok && file.Name == fmt.Sprint("files/crds/minimal/argoproj.io_", strings.TrimSuffix(name, ".argoproj.io"), ".yaml") {
					return true
				}
			}
			return false
		})
	}
}

// The prior release's omission list distinguishes missing borrowed definitions
// from definitions that its own chart can recreate.
func missingBorrowedDefinitions(missing []string, current definitionReview, history History) []string {
	shared, recorded := history.Latest.Values["sharedWorkflowDefinitions"].([]any)
	if history.Latest.Number == 0 || !recorded {
		if len(current.Shared) > 0 {
			return missing
		}
		return nil
	}
	var foreign []string
	for _, name := range missing {
		if slices.Contains(shared, any(name)) {
			foreign = append(foreign, name)
		}
	}
	return foreign
}
