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

type upgradeArgoSecurity struct {
	namespace, version string
	args               []string
	review             []ArgoSecurityReview
	consent            bool
}

func argoSecurityUpgradeRemedy(namespace string, cause error) error {
	args, _, _ := argoSecurityDefaults(nil)
	values := []string{}
	for i := 0; i+1 < len(args); i += 2 {
		values = append(values, args[i+1])
	}
	return fmt.Errorf("cannot reconcile Argo security defaults before deploying the Pod policy: %w. Set these exact native Argo Helm values in namespace %s:\n%s\nFor an existing Argo Helm release, approve the security changes in interactive review or pass --argo-security-defaults to fill absent fields only. Without an existing Argo Helm release, configure the live controller directly before retrying", cause, namespace, strings.Join(values, "\n"))
}

func planUpgradeArgoSecurity(ctx context.Context, deps Deps, values map[string]any, consent bool) (*upgradeArgoSecurity, error) {
	ns, _ := valueAt(values, "argo", "namespace").(string)
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	return prepareArgoSecurity(ctx, deps, ns, consent, false, nil)
}

func prepareArgoSecurity(ctx context.Context, deps Deps, ns string, consent, freshInstall bool, snapshot *helmRelease) (*upgradeArgoSecurity, error) {
	var release helmRelease
	var exists bool
	var err error
	if snapshot != nil {
		release = *snapshot
		exists = release.Name != ""
	} else {
		release, exists, err = findHelmRelease(ctx, deps, "argo-workflows", ns)
	}
	if err != nil {
		return nil, argoSecurityUpgradeRemedy(ns, errors.New("cannot inspect the Argo Helm release"))
	}
	stored, err := readStoredArgoSecurityValues(ctx, deps, ns, exists)
	if err != nil {
		return nil, argoSecurityUpgradeRemedy(ns, err)
	}
	args, review, err := argoSecurityDefaults(stored)
	if err != nil {
		return nil, argoSecurityUpgradeRemedy(ns, err)
	}
	if err := validateArgoSecurityReview(ns, stored, review); err != nil {
		return nil, err
	}
	plan := &upgradeArgoSecurity{namespace: ns, args: []string{}, consent: consent}
	// A fresh managed install supplies these values in the same run. Every
	// existing or external controller is judged by its actual ConfigMap.
	if exists || !freshInstall {
		liveErr := checkLiveArgoSecurity(ctx, deps, ns)
		if liveErr == nil {
			return plan, nil
		}
		if argoExternalControllerConfigMap(stored) {
			return nil, argoSecurityConfigMapRemedy(ns, liveErr, stored)
		}
		var missing *argoSecurityDefaultsMissing
		if !exists || !errors.As(liveErr, &missing) {
			return nil, argoSecurityConfigMapRemedy(ns, liveErr, stored)
		}
	}
	if err := validatePlannedArgoWorkflowSecurity(ns, stored, review); err != nil {
		return nil, err
	}
	version := chartVersionFromRelease(release.Chart, "argo-workflows")
	if exists && (canonicalChartVersion(version) == "" || release.Status != "deployed") {
		return nil, argoSecurityUpgradeRemedy(ns, fmt.Errorf("the Argo release must have a known chart version and deployed status"))
	}
	if deps.Output != nil {
		_, _ = fmt.Fprintf(deps.Output, "Argo security review: argo-workflows in namespace %s, chart %s; absent fields only.\n", ns, version)
	}
	printArgoSecurityReview(deps.Output, review)
	if exists && !consent {
		if deps.IsTerminal == nil || !deps.IsTerminal() || deps.Input == nil {
			return nil, argoSecurityUpgradeRemedy(ns, fmt.Errorf("live controller defaults are missing; existing Argo changes require explicit consent"))
		}
		if err := approveArgoSecurity(deps); err != nil {
			return nil, argoSecurityUpgradeRemedy(ns, err)
		}
		consent = true
	}
	if exists && len(args) == 0 {
		return nil, argoSecurityUpgradeRemedy(ns, fmt.Errorf("stored values already contain the defaults but the live controller does not; reconcile the controller configuration before retrying"))
	}
	return &upgradeArgoSecurity{namespace: ns, version: version, args: args, review: review, consent: consent}, nil
}

func validateArgoSecurityReview(ns string, stored map[string]any, review []ArgoSecurityReview) error {
	workflowPod, err := argoWorkflowPodSecurity(stored)
	if err != nil {
		return argoSecurityUpgradeRemedy(ns, err)
	}
	if err := validateArgoWorkflowPodSecurity("controller.workflowDefaults.spec.securityContext", workflowPod); err != nil {
		return argoSecurityUpgradeRemedy(ns, err)
	}
	// Explicit unsafe settings cannot be overwritten by this absent-only fill.
	// Refuse before either release changes rather than deploy a policy that
	// immediately strands ordinary Workflows or the controller itself.
	for _, entry := range review {
		if entry.Component == "server" {
			// The managed installer disables Argo Server; externally enabled servers
			// are checked through their own Helm values and chart defaults below.
			if enabled, ok := valueAt(stored, "server", "enabled").(bool); ok && !enabled {
				continue
			}
		}
		switch s := entry.Planned.(type) {
		case *corev1.PodSecurityContext:
			if !argoPodSecurityContextMeetsFloor(s) {
				return argoSecurityUpgradeRemedy(ns, fmt.Errorf("operator-set %s.%s cannot pass the Pod policy", entry.Component, entry.Field))
			}
		case *corev1.SecurityContext:
			valid := argoSecurityContextMeetsFloor(s, false)
			if (entry.Component == "mainContainer" || entry.Component == "executor") && workflowPod != nil && workflowPod.SeccompProfile != nil {
				valid = argoWorkflowSecurityContextMeetsFloor(s, workflowPod)
			}
			if !valid {
				return argoSecurityUpgradeRemedy(ns, fmt.Errorf("operator-set %s.%s cannot pass the Pod policy", entry.Component, entry.Field))
			}
		}
	}
	return nil
}

func argoSecurityContextMeetsFloor(s *corev1.SecurityContext, requireProfile bool) bool {
	if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || (s.Privileged != nil && *s.Privileged) || s.Capabilities == nil {
		return false
	}
	dropped := false
	for _, c := range s.Capabilities.Drop {
		dropped = dropped || c == "ALL"
	}
	for _, c := range s.Capabilities.Add {
		if c != "NET_BIND_SERVICE" {
			return false
		}
	}
	if s.SeccompProfile == nil {
		if requireProfile {
			return false
		}
	} else if s.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault && s.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost {
		return false
	}
	return dropped && (s.WindowsOptions == nil || s.WindowsOptions.HostProcess == nil || !*s.WindowsOptions.HostProcess)
}

func planUpgradeArgoSecurityPreview(ctx context.Context, deps Deps, raw []byte, consent bool) (*upgradeArgoSecurity, error) {
	var preview struct {
		Config map[string]any `json:"config"`
		Chart  struct {
			Values map[string]any `json:"values"`
		} `json:"chart"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&preview); err != nil {
		return nil, err
	}
	if preview.Chart.Values == nil {
		preview.Chart.Values = map[string]any{}
	}
	mergePlannedValues(preview.Chart.Values, preview.Config)
	return planUpgradeArgoSecurity(ctx, deps, preview.Chart.Values, consent)
}

func (plan *upgradeArgoSecurity) reconcile(ctx context.Context, deps Deps) error {
	if plan == nil {
		return nil
	}
	// Read again at apply. A concurrent operator override remains authoritative.
	fresh, err := planUpgradeArgoSecurity(ctx, deps, map[string]any{"argo": map[string]any{"namespace": plan.namespace}}, plan.consent)
	if err != nil {
		return err
	}
	if fresh == nil || len(fresh.args) == 0 {
		return nil
	}
	if err := refreshHelmRepo(ctx, deps, argoRepoName, ArgoHelmRepoURL); err != nil {
		return argoSecurityUpgradeRemedy(fresh.namespace, errors.New("cannot refresh the Argo chart repository"))
	}
	args := []string{"upgrade", "argo-workflows", argoRepoName + "/argo-workflows", "-n", fresh.namespace, "--version", fresh.version, "--reuse-values", "--wait"}
	args = append(args, fresh.args...)
	if _, err := deps.RunHelm(ctx, args); err != nil {
		return argoSecurityUpgradeRemedy(fresh.namespace, errors.New("native Argo security reconcile failed; inspect the Argo release"))
	}
	if err := checkLiveArgoSecurity(ctx, deps, fresh.namespace); err != nil {
		return argoSecurityUpgradeRemedy(fresh.namespace, err)
	}
	return nil
}
