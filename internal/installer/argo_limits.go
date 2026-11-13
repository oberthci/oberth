package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	DefaultArgoParallelism          = 4
	DefaultArgoNamespaceParallelism = 0
)

// validateArgoLimits checks the limit flags before anything is installed.
func validateArgoLimits(cfg Config) error {
	for name, value := range map[string]*int{"--argo-parallelism": cfg.ArgoParallelism, "--argo-namespace-parallelism": cfg.ArgoNamespaceParallelism} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s must be a non-negative integer (0 means no limit)", name)
		}
	}
	if argoLimitsRequested(cfg) && cfg.SkipArgo {
		return errors.New("--argo-parallelism and --argo-namespace-parallelism configure the managed argo-workflows release; set them on your own controller when --skip-argo-workflows is used")
	}
	return nil
}

// argoLimitsRequested reports whether this run changes any Argo limit, which
// forces a helm upgrade of an otherwise current argo-workflows release.
func argoLimitsRequested(cfg Config) bool {
	return cfg.ArgoParallelism != nil || cfg.ArgoNamespaceParallelism != nil
}

// argoLimitArgs renders the requested native controller cap.
func argoLimitArgs(cfg Config) []string {
	var args []string
	if cfg.ArgoParallelism != nil {
		args = append(args, "--set", fmt.Sprintf("controller.parallelism=%d", *cfg.ArgoParallelism))
	}
	if cfg.ArgoNamespaceParallelism != nil {
		args = append(args, "--set", fmt.Sprintf("controller.namespaceParallelism=%d", *cfg.ArgoNamespaceParallelism))
	}
	return args
}

// noticeArgoExternalConfigMap explains why native controller settings cannot
// change a ConfigMap supplied outside the chart.
func noticeArgoExternalConfigMap(ctx context.Context, deps Deps, namespace string, exists bool) error {
	if !exists {
		return nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		return fmt.Errorf("read Argo controller ConfigMap setting: %w", err)
	}
	var stored struct {
		Controller struct {
			ConfigMap struct {
				Create any `json:"create"`
			} `json:"configMap"`
		} `json:"controller"`
	}
	if err := json.Unmarshal(nonEmptyJSON(raw), &stored); err != nil {
		return fmt.Errorf("decode Argo controller ConfigMap setting: %w", err)
	}
	if create, ok := stored.Controller.ConfigMap.Create.(bool); ok && !create {
		_, _ = fmt.Fprintln(deps.Output, "Argo controller.configMap.create=false: controller.parallelism and executor.resources values do not change the external ConfigMap; migrate the old profile settings as described in docs/releases/v0.22.5.md.")
	}
	return nil
}
