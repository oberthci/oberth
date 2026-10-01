package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// nonrootArgoValues uses existing fields of the pinned upstream chart. The
// immutable ConfigMap is part of the same Helm release as the controller, so
// ordinary Helm object ordering creates it before the Deployment.
func nonrootArgoValues(namespace string) map[string]any {
	return map[string]any{
		"controller": map[string]any{
			"configMap": map[string]any{"create": false, "name": argoworkflow.NonrootProfileConfigMap},
			"image":     map[string]any{"tag": strings.SplitN(argoworkflow.NonrootControllerImage, ":", 2)[1]},
			"volumes": []any{map[string]any{"name": argoworkflow.NonrootProfileVolume, "configMap": map[string]any{
				"name": argoworkflow.NonrootProfileConfigMap, "optional": false, "defaultMode": 0444,
				"items": []any{map[string]any{"key": "config", "path": "config"}},
			}}},
			"volumeMounts": []any{map[string]any{"name": argoworkflow.NonrootProfileVolume, "mountPath": argoworkflow.NonrootProfileMount, "readOnly": true}},
		},
		"executor": map[string]any{"image": map[string]any{"tag": strings.SplitN(argoworkflow.NonrootExecutorImage, ":", 2)[1]}},
		"extraObjects": []any{map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata":  map[string]any{"name": argoworkflow.NonrootProfileConfigMap, "namespace": namespace},
			"immutable": true, "data": map[string]any{"config": argoworkflow.NonrootProfileConfig},
		}},
	}
}

// validateNonrootReuse admits only the ordinary installer options or the exact
// built-in profile. It rejects customization instead of silently overwriting
// archive, plugin, controller or environment settings under --reuse-values.
func validateNonrootReuse(values map[string]any, namespace string) error {
	allowed := map[string]any{
		"singleNamespace": true, "createAggregateRoles": false,
		"server":   map[string]any{"enabled": false},
		"workflow": map[string]any{"serviceAccount": map[string]any{"create": false}, "rbac": map[string]any{"create": false}},
		"crds":     map[string]any{"install": true, "keep": true, "full": false},
		"controller": map[string]any{
			"clusterWorkflowTemplates": map[string]any{"enabled": false},
			"extraEnv":                 []any{map[string]any{"name": podNamesEnv, "value": podNamesFormat}},
		},
	}
	profile := nonrootArgoValues(namespace)
	for key, value := range profile {
		if key == "controller" {
			for child, v := range value.(map[string]any) {
				allowed[key].(map[string]any)[child] = v
			}
		} else {
			allowed[key] = value
		}
	}
	var subset func(map[string]any, map[string]any) bool
	subset = func(actual, want map[string]any) bool {
		for key, value := range actual {
			expected, ok := want[key]
			if !ok {
				return false
			}
			if nested, ok := value.(map[string]any); ok {
				other, ok := expected.(map[string]any)
				if !ok || !subset(nested, other) {
					return false
				}
			} else {
				// Helm's JSON numbers decode as float64; compare canonical JSON
				// so the exact supported integer file modes survive reuse.
				actualJSON, actualErr := json.Marshal(value)
				expectedJSON, expectedErr := json.Marshal(expected)
				if actualErr != nil || expectedErr != nil || !bytes.Equal(actualJSON, expectedJSON) {
					return false
				}
			}
		}
		return true
	}
	if !subset(values, allowed) {
		return errors.New("nonroot profile refuses incompatible reused/custom Argo settings; review the existing controller configuration before installing this profile")
	}
	return nil
}

func prepareNonrootArgoValues(ctx context.Context, cfg Config, deps Deps, exists bool, installedVersion string) (string, func(), error) {
	noop := func() {}
	if cfg.ArgoControllerProfile == "" {
		return "", noop, nil
	}
	if cfg.ArgoControllerProfile != argoworkflow.NonrootStaticProfile ||
		(cfg.ArgoChartVersion != "" && cfg.ArgoChartVersion != DefaultArgoChartVersion) ||
		(exists && installedVersion != DefaultArgoChartVersion) || cfg.SkipArgo {
		return "", noop, errors.New("nonroot profile requires the managed exact Argo chart1.0.24")
	}
	if exists {
		encoded, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", cfg.ArgoNamespace, "-o", "json"})
		if err != nil {
			return "", noop, errors.New("cannot inspect existing Argo profile settings")
		}
		var values map[string]any
		if err := json.Unmarshal(encoded, &values); err != nil {
			return "", noop, errors.New("cannot decode existing Argo profile settings")
		}
		if err := validateNonrootReuse(values, cfg.ArgoNamespace); err != nil {
			return "", noop, err
		}
	}
	encoded, err := json.Marshal(nonrootArgoValues(cfg.ArgoNamespace))
	if err != nil {
		return "", noop, fmt.Errorf("encode built-in Argo profile: %w", err)
	}
	file, err := os.CreateTemp("", "oberth-argo-profile-*.json")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		cleanup()
		return "", noop, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", noop, err
	}
	return file.Name(), cleanup, nil
}
