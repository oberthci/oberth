package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// ArgoExecutorResourcesReview describes the native controller setting chosen for
// install. Stored operator values are retained as supplied, including an empty
// resources map; they are never completed with installer defaults.
type ArgoExecutorResourcesReview struct {
	// OperatorSet marks a stored setting; its original writer is unknown.
	OperatorSet bool `json:"operator_set"`
	Resources   any  `json:"resources"`
}

// readArgoExecutorResources reads user-supplied values, excluding chart defaults.
// Only an absent executor.resources permits the installer to add its budgets.
func readArgoExecutorResources(ctx context.Context, deps Deps, namespace string, exists bool) (*ArgoExecutorResourcesReview, error) {
	if !exists {
		return nil, nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		return nil, fmt.Errorf("read stored Argo executor resources: %w", err)
	}
	var values map[string]any
	raw = nonEmptyJSON(raw)
	if !json.Valid(raw) {
		return nil, errors.New("decode stored Argo executor resources: invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode stored Argo executor resources: %w", err)
	}
	executor, exists := values["executor"]
	if !exists {
		return nil, nil
	}
	fields, object := executor.(map[string]any)
	if !object {
		return &ArgoExecutorResourcesReview{OperatorSet: true}, nil
	}
	resources, exists := fields["resources"]
	if !exists {
		return nil, nil
	}
	return &ArgoExecutorResourcesReview{OperatorSet: true, Resources: resources}, nil
}

func resolveArgoExecutorResources(ctx context.Context, cfg *Config, deps Deps) error {
	if cfg.SkipArgo {
		return nil
	}
	namespace := cfg.ArgoNamespace
	if namespace == "" {
		namespace = DefaultArgoNamespace
	}
	exists := cfg.existingArgoRelease
	if !cfg.argoReleaseRead {
		release, found, err := findHelmRelease(ctx, deps, "argo-workflows", namespace)
		if err != nil {
			return err
		}
		exists = found
		cfg.existingArgoRelease, cfg.argoReleaseRead = exists, true
		cfg.argoReviewRelease = &release
	}
	resources, err := readArgoExecutorResources(ctx, deps, namespace, exists)
	cfg.argoExecutorResources = resources
	return err
}

func printArgoExecutorResources(w io.Writer, review *ArgoExecutorResourcesReview) {
	if w == nil || review == nil {
		return
	}
	label := "installer defaults"
	if review.OperatorSet {
		label = "stored value kept"
	}
	resources, _ := json.Marshal(review.Resources)
	_, _ = fmt.Fprintf(w, "Argo executor.resources: %s %s\n", label, resources)
}

func argoExecutorResourcesForReview(cfg Config) *ArgoExecutorResourcesReview {
	if cfg.SkipArgo {
		return nil
	}
	if cfg.argoExecutorResources != nil {
		return cfg.argoExecutorResources
	}
	return &ArgoExecutorResourcesReview{Resources: argoworkflow.ExecutorResources()}
}

// A complete native resources value is installed only when the operator has
// not stored one. --reuse-values keeps every other Argo setting unchanged.
func argoExecutorResourceArgs(cfg Config) []string {
	review := argoExecutorResourcesForReview(cfg)
	if review == nil || review.OperatorSet {
		return nil
	}
	resources, _ := json.Marshal(review.Resources)
	return []string{"--set-json", "executor.resources=" + string(resources)}
}
