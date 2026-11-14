package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// ArgoWorkflowTTLReview describes the native default. Authored Workflows may
// override it. An existing operator setting is preserved, including null or {}.
type ArgoWorkflowTTLReview struct {
	OperatorSet bool `json:"operator_set"`
	TTLStrategy any  `json:"ttl_strategy"`
}

func readArgoWorkflowTTL(ctx context.Context, deps Deps, namespace string, exists bool) (*ArgoWorkflowTTLReview, error) {
	if !exists {
		return nil, nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		return nil, fmt.Errorf("read Argo workflowDefaults: %w", err)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(nonEmptyJSON(raw)))
	decoder.UseNumber()
	if !json.Valid(nonEmptyJSON(raw)) {
		return nil, fmt.Errorf("read Argo workflowDefaults: invalid JSON")
	}
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("read Argo workflowDefaults: %w", err)
	}
	var current any = values
	for _, key := range []string{"controller", "workflowDefaults", "spec", "ttlStrategy"} {
		object, ok := current.(map[string]any)
		if !ok {
			return &ArgoWorkflowTTLReview{OperatorSet: true, TTLStrategy: current}, nil
		}
		current, ok = object[key]
		if !ok {
			return nil, nil
		}
	}
	return &ArgoWorkflowTTLReview{OperatorSet: true, TTLStrategy: current}, nil
}

func argoWorkflowTTLForReview(cfg Config) *ArgoWorkflowTTLReview {
	if cfg.SkipArgo {
		return nil
	}
	if cfg.argoWorkflowTTL != nil {
		return cfg.argoWorkflowTTL
	}
	return &ArgoWorkflowTTLReview{TTLStrategy: map[string]any{"secondsAfterCompletion": 3600}}
}

func printArgoWorkflowTTL(w io.Writer, review *ArgoWorkflowTTLReview) {
	if w == nil || review == nil {
		return
	}
	label := "installer default; authored cleanup settings take precedence"
	if review.OperatorSet {
		label = "stored value kept"
	}
	value, _ := json.Marshal(review.TTLStrategy)
	_, _ = fmt.Fprintf(w, "Argo controller.workflowDefaults.spec.ttlStrategy: %s %s\n", label, value)
}

func argoWorkflowTTLArgs(cfg Config) []string {
	review := argoWorkflowTTLForReview(cfg)
	if review == nil || review.OperatorSet {
		return nil
	}
	value, _ := json.Marshal(review.TTLStrategy)
	return []string{"--set-json", "controller.workflowDefaults.spec.ttlStrategy=" + string(value)}
}

// Ordinary upgrades review and fill the native cleanup default before the server
// stops supplying per-Workflow cleanup. Explicit operator values always win.
func planUpgradeWorkflowCleanup(ctx context.Context, deps Deps, namespace string, plan *upgradeArgoSecurity) (*upgradeArgoSecurity, error) {
	if plan != nil {
		plan.workflowCleanup = true
	}
	release, exists, err := findHelmRelease(ctx, deps, "argo-workflows", namespace)
	if err != nil {
		return nil, fmt.Errorf("inspect Argo workflow cleanup: %w", err)
	}
	if !exists {
		return plan, nil
	}
	review, err := readArgoWorkflowTTL(ctx, deps, namespace, true)
	if err != nil {
		return nil, err
	}
	cfg := Config{argoWorkflowTTL: review}
	printArgoWorkflowTTL(deps.Output, argoWorkflowTTLForReview(cfg))
	args := argoWorkflowTTLArgs(cfg)
	if len(args) == 0 {
		return plan, nil
	}
	version := chartVersionFromRelease(release.Chart, "argo-workflows")
	if canonicalChartVersion(version) == "" || release.Status != "deployed" {
		return nil, fmt.Errorf("argo workflow cleanup requires a deployed release with a known chart version")
	}
	if plan == nil {
		plan = &upgradeArgoSecurity{namespace: namespace, workflowCleanup: true}
	}
	plan.version = version
	plan.args = append(plan.args, args...)
	return plan, nil
}
