package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ArgoSecurityReview shows native Helm defaults before and after the install.
// These fields contain only Kubernetes security contexts, never credentials.
type ArgoSecurityReview struct {
	Component string `json:"component"`
	Field     string `json:"field"`
	Current   any    `json:"current"`
	Planned   any    `json:"planned"`
}

// Existing leaves (including explicit nulls and empty lists) belong to the
// operator. Fill absent fields only; an unsafe explicit value remains visible
// in review and the cluster policy refuses Pods that use it.
func argoSecurityDefaults(values map[string]any) ([]string, []ArgoSecurityReview, error) {
	workflowPod, err := argoWorkflowPodSecurity(values)
	if err != nil {
		return nil, nil, err
	}
	args := []string{}
	review := []ArgoSecurityReview{}
	for _, target := range []struct {
		component, field string
		pod              bool
	}{
		{"mainContainer", "securityContext", false}, {"executor", "securityContext", false},
		{"controller", "podSecurityContext", true}, {"controller", "securityContext", false},
		{"server", "podSecurityContext", true}, {"server", "securityContext", false},
	} {
		component, field := target.component, target.field
		current, err := json.Marshal(valueAt(values, component, field))
		if err != nil {
			return nil, nil, err
		}
		entry := ArgoSecurityReview{Component: component, Field: field}
		decode := func(raw []byte) (any, error) {
			if target.pod {
				var context *corev1.PodSecurityContext
				err := json.Unmarshal(raw, &context)
				return context, err
			}
			var context *corev1.SecurityContext
			err := json.Unmarshal(raw, &context)
			return context, err
		}
		entry.Current, err = decode(current)
		if err != nil {
			return nil, nil, fmt.Errorf("decode Argo %s.%s: %w", component, field, err)
		}
		// Work on a private copy; never change the stored values snapshot.
		raw, err := json.Marshal(values)
		if err != nil {
			return nil, nil, err
		}
		var planned map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&planned); err != nil {
			return nil, nil, err
		}
		if planned == nil {
			planned = map[string]any{}
		}
		defaults := []struct {
			path  []string
			value any
		}{
			{[]string{"allowPrivilegeEscalation"}, false},
			{[]string{"privileged"}, false},
			{[]string{"capabilities", "drop"}, []string{"ALL"}},
			{[]string{"seccompProfile", "type"}, "RuntimeDefault"},
		}
		if target.pod {
			defaults = defaults[3:]
		} else if (component == "mainContainer" || component == "executor") && workflowPod != nil && workflowPod.SeccompProfile != nil && allowedArgoSeccomp(workflowPod.SeccompProfile) {
			defaults = defaults[:3]
		}
		for _, entry := range defaults {
			path := append([]string{component, field}, entry.path...)
			if setAbsentArgoSecurityValue(planned, path, entry.value) {
				encoded, err := json.Marshal(entry.value)
				if err != nil {
					return nil, nil, err
				}
				args = append(args, "--set-json", strings.Join(path, ".")+"="+string(encoded))
			}
		}
		next, err := json.Marshal(valueAt(planned, component, field))
		if err != nil {
			return nil, nil, err
		}
		entry.Planned, err = decode(next)
		if err != nil {
			return nil, nil, err
		}
		review = append(review, entry)
	}
	return args, review, nil
}

func setAbsentArgoSecurityValue(values map[string]any, path []string, value any) bool {
	if len(path) == 1 {
		if _, exists := values[path[0]]; exists {
			return false
		}
		values[path[0]] = value
		return true
	}
	child, exists := values[path[0]]
	if !exists {
		child = map[string]any{}
		values[path[0]] = child
	}
	next, ok := child.(map[string]any)
	return ok && setAbsentArgoSecurityValue(next, path[1:], value)
}

func readStoredArgoSecurityValues(ctx context.Context, deps Deps, ns string, exists bool) (map[string]any, error) {
	values := map[string]any{}
	if exists {
		// Raw stored values distinguish an explicit null from an absent field.
		raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", ns, "-o", "json"})
		if err != nil {
			return nil, errors.New("cannot read stored Argo security defaults")
		}
		raw = bytes.Clone(raw)
		defer clear(raw)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid(raw) || decoder.Decode(&values) != nil {
			return nil, errors.New("cannot decode stored Argo security defaults")
		}
	}
	return values, nil
}

func readArgoSecurityReview(ctx context.Context, cfg *Config, deps Deps) error {
	cfg.argoSecurityArgs = nil
	cfg.argoSecurityReview = nil
	ns := cfg.ArgoNamespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	snapshot := cfg.argoReviewRelease
	if !cfg.argoReleaseRead {
		snapshot = nil
	}
	if cfg.DryRun {
		deps.Input, deps.IsTerminal = nil, nil
	}
	plan, err := prepareArgoSecurity(ctx, deps, ns, cfg.ArgoSecurityDefaults, !cfg.SkipArgo, snapshot)
	if err != nil {
		return err
	}
	cfg.argoSecurityArgs, cfg.argoSecurityReview = plan.args, plan.review
	cfg.ArgoSecurityDefaults = plan.consent
	return nil
}
