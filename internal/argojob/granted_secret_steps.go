package argojob

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// GrantedSecretSteps is the single admission and delivery definition of a
// granted step. Build also authorizes declaredPaths against the approval
// table for the exact repository/upstream before delivery. A main store consumer gets
// settings only when every consumed path is declared and granted to its stable
// template name or "*". Nil grants confer no authority. Token opt-outs and
// commands without paths get no settings. Sidecars/init/Argo helpers are never
// consumers, regardless of their commands.
func GrantedSecretSteps(workflow *wfv1.Workflow, declaredPaths []string, stepGrants map[string]map[string]bool) (map[*wfv1.Template]bool, error) {
	argoworkflow.InheritTokenOptOut(workflow)
	grantedSteps := make(map[*wfv1.Template]bool)
	declared := make(map[string]struct{}, len(declaredPaths))
	for _, p := range declaredPaths {
		declared[p] = struct{}{}
	}
	var problems []error
	// Inline templates are visited too: the credential mount injection reaches
	// them, so the admission that governs the same invocation must as well.
	walkTemplates(workflow, func(template *wfv1.Template) {
		if !templateUsesCredentialChain(template) {
			return
		}
		execPaths := extractExecPaths(template)
		if len(execPaths) == 0 || tokenDisabled(template) {
			return
		}
		problemCount := len(problems)
		if len(declaredPaths) == 0 {
			problems = append(problems, fmt.Errorf(
				"argojob: template %q uses oberth secretstore exec with --path flags "+
					"but the workflow declares no %s annotation",
				template.Name, argoworkflow.SecretPathsAnnotation))
			return
		}
		// Determine whether this template has a stable, unambiguous name
		// suitable for per-step grant matching: it must be a top-level
		// template (not inline) and have a non-empty name.
		grantName := secretStepGrantName(workflow, template)
		stableName := grantName != "*"
		for _, execPath := range execPaths {
			if _, ok := declared[execPath]; !ok {
				problems = append(problems, fmt.Errorf(
					"argojob: template %q declares --path %q which is not in the "+
						"workflow's %s annotation",
					template.Name, execPath, argoworkflow.SecretPathsAnnotation))
				continue
			}
			// Every path needs an explicit step grant, including wildcard access.
			steps := stepGrants[execPath]
			if steps["*"] {
				// Wildcard grant covers all templates.
				continue
			}
			if !stableName {
				// Fail closed: a path with only named-step grants cannot
				// be consumed by an inline or unnamed
				// template because the name is not stable or authoritative.
				problems = append(problems, fmt.Errorf(
					"secret grant denied: step %q not authorized for path %q "+
						"-- template does not have a stable name (inline or unnamed); "+
						"only wildcard grants (step \"*\") may authorize these templates",
					template.Name, execPath))
				continue
			}
			if steps[grantName] {
				// Exact named-step match.
				continue
			}
			// Named-step grant exists but does not match this template.
			var granted []string
			for s := range steps {
				granted = append(granted, s)
			}
			sort.Strings(granted)
			problems = append(problems, fmt.Errorf(
				"secret grant denied: step %q not authorized for path %q "+
					"-- granted to step %q only",
				template.Name, execPath, strings.Join(granted, "\", \"")))
		}
		if len(problems) == problemCount {
			grantedSteps[template] = true
		}
	})
	return grantedSteps, errors.Join(problems...)
}

// secretStepGrantName is shared by admission and identity selection. Inline,
// unnamed consumers can use only the file's wildcard
// identity, even if their display name happens to match a named step grant.
func secretStepGrantName(workflow *wfv1.Workflow, template *wfv1.Template) string {
	if template.Name == "" {
		return "*"
	}
	for i := range workflow.Spec.Templates {
		if template == &workflow.Spec.Templates[i] {
			return template.Name
		}
	}
	return "*"
}

// injectGrantedSecretSettings uses the admission decision without reclassifying
// commands or granting authority to other containers in the same Pod.
func injectGrantedSecretSettings(t *wfv1.Template, granted map[*wfv1.Template]bool, mounts []corev1.VolumeMount, environment []corev1.EnvVar, depth int) {
	if t == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	if granted[t] {
		if c := templateMainContainer(t); c != nil {
			c.VolumeMounts = append(c.VolumeMounts, mounts...)
			c.Env = overrideEnvironment(c.Env, environment)
		}
	}
	for group := range t.Steps {
		for step := range t.Steps[group].Steps {
			injectGrantedSecretSettings(t.Steps[group].Steps[step].Inline, granted, mounts, environment, depth+1)
		}
	}
	if t.DAG != nil {
		for task := range t.DAG.Tasks {
			injectGrantedSecretSettings(t.DAG.Tasks[task].Inline, granted, mounts, environment, depth+1)
		}
	}
}

// SecretStoreRequested reports whether any main step declares store reads.
// This determines whether to look up current grants, never whether to deliver
// settings. GrantedSecretSteps makes that authorization decision.
func SecretStoreRequested(workflow *wfv1.Workflow) bool {
	argoworkflow.InheritTokenOptOut(workflow)
	_, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		return false
	}
	requested := false
	walkTemplates(workflow, func(t *wfv1.Template) {
		requested = requested || (templateUsesCredentialChain(t) && len(extractExecPaths(t)) > 0 && !tokenDisabled(t))
	})
	return requested
}
