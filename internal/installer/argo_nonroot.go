package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// nonrootArgoValues uses existing fields of the pinned upstream chart. The
// immutable ConfigMap is part of the same Helm release as the controller, so
// ordinary Helm object ordering creates it before the Deployment. The
// profile carries the controller's run limits: with configMap.create false
// the chart never renders controller.parallelism itself (#1517).
func nonrootArgoValues(namespace string, profile argoworkflow.NonrootProfile) map[string]any {
	return map[string]any{
		"controller": map[string]any{
			"configMap": map[string]any{"create": false, "name": profile.ConfigMap},
			"image":     map[string]any{"tag": strings.SplitN(argoworkflow.NonrootControllerImage, ":", 2)[1]},
			"volumes": []any{map[string]any{"name": argoworkflow.NonrootProfileVolume, "configMap": map[string]any{
				"name": profile.ConfigMap, "optional": false, "defaultMode": 0444,
				"items": []any{map[string]any{"key": "config", "path": "config"}},
			}}},
			"volumeMounts": []any{map[string]any{"name": argoworkflow.NonrootProfileVolume, "mountPath": argoworkflow.NonrootProfileMount, "readOnly": true}},
		},
		"executor":     map[string]any{"image": map[string]any{"tag": strings.SplitN(argoworkflow.NonrootExecutorImage, ":", 2)[1]}},
		"extraObjects": nonrootProfileObjects(namespace, profile),
	}
}

// baseNonrootProfile is the built-in profile without controller limits.
func baseNonrootProfile() argoworkflow.NonrootProfile {
	profile, _ := argoworkflow.NewNonrootProfile(0, 0) // Zero limits are always valid.
	return profile
}

// storedParallelism reads controller.<key> from helm get values output.
func storedParallelism(values map[string]any, key string) int {
	controller, _ := values["controller"].(map[string]any)
	value, _ := controller[key].(float64)
	if value < 0 || value > argoworkflow.MaxNonrootParallelism || value != math.Trunc(value) {
		return 0
	}
	return int(value)
}

// effectiveNonrootProfile is the profile for this run's limits: a requested
// flag wins, otherwise the stored release value, otherwise none.
func effectiveNonrootProfile(cfg Config, stored map[string]any) (argoworkflow.NonrootProfile, error) {
	parallelism, namespaced := storedParallelism(stored, "parallelism"), storedParallelism(stored, "namespaceParallelism")
	if cfg.ArgoParallelism != nil {
		parallelism = *cfg.ArgoParallelism
	}
	if cfg.ArgoNamespaceParallelism != nil {
		namespaced = *cfg.ArgoNamespaceParallelism
	}
	return argoworkflow.NewNonrootProfile(parallelism, namespaced)
}

// nonrootProfile is the profile ArgoHelmArgs renders: the one the Argo
// phase computed, or one from the requested limits alone (dry-run plans).
func (cfg Config) nonrootProfile() argoworkflow.NonrootProfile {
	if cfg.argoProfile != nil {
		return *cfg.argoProfile
	}
	profile, err := effectiveNonrootProfile(cfg, nil)
	if err != nil {
		return baseNonrootProfile()
	}
	return profile
}

// nonrootProfileObjects is the profile's extraObjects list: the immutable
// controller ConfigMap. The limits ConfigMap (argo_limits.go) shares the
// list, so every writer of extraObjects starts from this.
func nonrootProfileObjects(namespace string, profile argoworkflow.NonrootProfile) []any {
	return []any{map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":  map[string]any{"name": profile.ConfigMap, "namespace": namespace},
		"immutable": true, "data": map[string]any{"config": profile.Config},
	}}
}

// validateNonrootReuse admits only the ordinary installer options or the exact
// built-in profile, plus controller priority and CPU/memory requests. It rejects
// other customization instead of silently overwriting
// archive, plugin, controller or environment settings under --reuse-values.
// checkNonrootReuse accepts stored Argo values the nonroot profile can be
// layered over: the built-in allowlist, or values outside it that the
// operator reviewed and acknowledged with --argo-values-reviewed, the SHA-256
// of exactly this `helm get values argo-workflows -o json` output. The
// acknowledgment binds the bytes reviewed: any later change refuses again.
// The controller's image, arguments, environment and mounts are still
// verified against the profile after the upgrade.
func checkNonrootReuse(raw []byte, values map[string]any, namespace, reviewed string) error {
	err := validateNonrootReuse(values, namespace)
	if err == nil {
		return nil
	}
	if reviewed == "" {
		return fmt.Errorf("%w; to keep them, review `helm get values argo-workflows -n %s -o json` and pass "+
			"--argo-values-reviewed sha256:<SHA-256 of that output>", err, namespace)
	}
	sum := sha256.Sum256(raw)
	if strings.TrimPrefix(reviewed, "sha256:") != hex.EncodeToString(sum[:]) {
		return errors.New("--argo-values-reviewed does not match the stored Argo values: they changed since the review, or the digest is not of `helm get values argo-workflows -o json`")
	}
	return nil
}

func validateNonrootReuse(values map[string]any, namespace string) error {
	// The stored profile is the canonical one for the stored limits, or the
	// built-in one an earlier release wrote before the limits were set.
	candidates := []argoworkflow.NonrootProfile{baseNonrootProfile()}
	if stored, err := argoworkflow.NewNonrootProfile(storedParallelism(values, "parallelism"), storedParallelism(values, "namespaceParallelism")); err == nil && stored.ConfigMap != candidates[0].ConfigMap {
		candidates = append([]argoworkflow.NonrootProfile{stored}, candidates...)
	}
	var err error
	for _, profile := range candidates {
		if err = validateNonrootReuseFor(values, namespace, profile); err == nil {
			return nil
		}
	}
	return err
}

func validateNonrootReuseFor(values map[string]any, namespace string, nonroot argoworkflow.NonrootProfile) error {
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
	// These platform overlay fields do not change controller/executor identity.
	// Keep the nested allowlist exact: limits and other resource keys stay refused.
	controllerAllowed := allowed["controller"].(map[string]any)
	if controller, ok := values["controller"].(map[string]any); ok {
		if value, exists := controller["priorityClassName"]; exists {
			name, ok := value.(string)
			if !ok || (name != "" && len(validation.IsDNS1123Subdomain(name)) != 0) {
				return errors.New("nonroot profile requires a valid controller.priorityClassName")
			}
			controllerAllowed["priorityClassName"] = value
		}
		// Placement only: where the controller runs, not what it runs.
		if value, exists := controller["nodeSelector"]; exists {
			selector, ok := value.(map[string]any)
			for _, v := range selector {
				if _, isString := v.(string); !isString {
					ok = false
				}
			}
			if !ok {
				return errors.New("nonroot profile requires controller.nodeSelector to map labels to strings")
			}
			controllerAllowed["nodeSelector"] = value
		}
		if value, exists := controller["tolerations"]; exists {
			if _, ok := value.([]any); !ok {
				return errors.New("nonroot profile requires controller.tolerations to be a list")
			}
			controllerAllowed["tolerations"] = value
		}
		if resources, ok := controller["resources"].(map[string]any); ok {
			requests, _ := resources["requests"].(map[string]any)
			allowedRequests := map[string]any{}
			for _, key := range []string{"cpu", "memory"} {
				if value, exists := requests[key]; exists {
					text, ok := value.(string)
					quantity, err := resource.ParseQuantity(text)
					if !ok || err != nil || quantity.Sign() <= 0 {
						return fmt.Errorf("nonroot profile requires a positive controller.resources.requests.%s quantity string", key)
					}
					allowedRequests[key] = value
				}
			}
			controllerAllowed["resources"] = map[string]any{"requests": allowedRequests}
		}
		// The run limits this installer writes (#1517) are not customization:
		// the controller's own parallelism, non-negative whole numbers.
		for _, key := range []string{"parallelism", "namespaceParallelism"} {
			if value, exists := controller[key]; exists {
				n, ok := value.(float64)
				if !ok || n < 0 || n > maxArgoParallelism || n != math.Trunc(n) {
					return fmt.Errorf("nonroot profile requires a non-negative integer controller.%s", key)
				}
				controllerAllowed[key] = value
			}
		}
	}
	// The limits ConfigMap the installer wrote beside the profile object is
	// its own, so the comparison below sees extraObjects without it.
	if objects, ok := values["extraObjects"].([]any); ok {
		rest := make([]any, 0, len(objects))
		for _, object := range objects {
			if !isSyncLimitsConfigMap(object, namespace) {
				rest = append(rest, object)
			}
		}
		values = maps.Clone(values)
		if len(rest) == 0 {
			delete(values, "extraObjects")
		} else {
			values["extraObjects"] = rest
		}
	}
	profile := nonrootArgoValues(namespace, nonroot)
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

func prepareNonrootArgoValues(ctx context.Context, cfg Config, deps Deps, exists bool, installedVersion string) (string, *argoworkflow.NonrootProfile, func(), error) {
	noop := func() {}
	if cfg.ArgoControllerProfile == "" {
		return "", nil, noop, nil
	}
	if cfg.ArgoControllerProfile != argoworkflow.NonrootStaticProfile ||
		(cfg.ArgoChartVersion != "" && cfg.ArgoChartVersion != DefaultArgoChartVersion) ||
		(exists && installedVersion != DefaultArgoChartVersion) || cfg.SkipArgo {
		return "", nil, noop, errors.New("nonroot profile requires the managed exact Argo chart1.0.24")
	}
	var stored map[string]any
	if exists {
		encoded, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", cfg.ArgoNamespace, "-o", "json"})
		if err != nil {
			return "", nil, noop, errors.New("cannot inspect existing Argo profile settings")
		}
		if err := json.Unmarshal(encoded, &stored); err != nil {
			return "", nil, noop, errors.New("cannot decode existing Argo profile settings")
		}
		if err := checkNonrootReuse(encoded, stored, cfg.ArgoNamespace, cfg.ArgoValuesReviewed); err != nil {
			return "", nil, noop, err
		}
	}
	nonroot, err := effectiveNonrootProfile(cfg, stored)
	if err != nil {
		return "", nil, noop, err
	}
	profile := nonrootArgoValues(cfg.ArgoNamespace, nonroot)
	if cfg.ArgoSyncLimits == nil {
		// This values file replaces the release's extraObjects. Keep the
		// limits ConfigMap an earlier run wrote there (#1517): a list without
		// it would make Helm delete it under the Workflows whose semaphores
		// name it. A requested limit set goes through --set-json instead.
		if object := storedSyncLimitsConfigMap(stored, cfg.ArgoNamespace); object != nil {
			profile["extraObjects"] = append(profile["extraObjects"].([]any), object)
		}
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return "", nil, noop, fmt.Errorf("encode built-in Argo profile: %w", err)
	}
	file, err := os.CreateTemp("", "oberth-argo-profile-*.json")
	if err != nil {
		return "", nil, noop, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, noop, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, noop, err
	}
	return file.Name(), &nonroot, cleanup, nil
}

// storedNonrootRelease reports whether the existing argo-workflows release
// runs a nonroot controller profile ConfigMap.
func storedNonrootRelease(ctx context.Context, deps Deps, namespace string) (bool, error) {
	encoded, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		return false, errors.New("cannot inspect existing Argo profile settings")
	}
	var stored struct {
		Controller struct {
			ConfigMap struct {
				Name string `json:"name"`
			} `json:"configMap"`
		} `json:"controller"`
	}
	if len(bytes.TrimSpace(encoded)) == 0 || string(bytes.TrimSpace(encoded)) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return false, errors.New("cannot decode existing Argo profile settings")
	}
	return argoworkflow.ValidNonrootProfileConfigMap(stored.Controller.ConfigMap.Name), nil
}
