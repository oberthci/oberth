package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// ReleaseDeclaration contains public intent only; observing it grants no access.
type ReleaseDeclaration struct{ Step, Path string }

type ReleasePreflight struct {
	Declarations []ReleaseDeclaration
	Problems     []string
}

// PreflightRelease checks public capabilities without building a credentialed
// Workflow, seeding source, requesting tokens, or contacting the secret store.
func PreflightRelease(config Config, request Request) (ReleasePreflight, error) {
	result := ReleasePreflight{}
	if request.Identities != nil {
		config.PerRepoIdentities = request.Identities.Release
		config.PerRepoCIIdentities = request.Identities.CI
		config.PerStepIdentities = request.Identities.Step
	}
	request.Trigger = periapsis.TriggerRelease
	wf, err := argoworkflow.Decode(request.Source)
	if err != nil {
		return result, err
	}
	if _, err = argoworkflow.Resolve(wf, request.Fragments); err != nil {
		return result, err
	}
	paths, err := argoworkflow.DeclaredSecretPaths(wf)
	if err != nil {
		return result, err
	}
	add := func(err error) {
		if err != nil {
			result.Problems = append(result.Problems, err.Error())
		}
	}
	top := map[*wfv1.Template]bool{}
	for i := range wf.Spec.Templates {
		top[&wf.Spec.Templates[i]] = true
	}
	seen := map[string]bool{}
	consumed := map[string]bool{}
	declare := func(step, path string) {
		if seen[step+"\x00"+path] {
			return
		}
		seen[step+"\x00"+path] = true
		consumed[path] = true
		result.Declarations = append(result.Declarations, ReleaseDeclaration{step, path})
		grants := request.StepGrants[path]
		if grants["*"] || grants[step] {
			return
		}
		args, _ := json.Marshal(map[string]string{"repo": canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo), "step": step, "secret": path})
		result.Problems = append(result.Problems, fmt.Sprintf("step %q lacks a grant for %q; run access_allow %s", step, path, args))
	}
	walkTemplates(wf, func(t *wfv1.Template) {
		step := t.Name
		if !top[t] || step == "" || strings.HasPrefix(step, "frag-") {
			step = "*"
		}
		for _, path := range extractExecPaths(t) {
			declare(step, path)
		}
		if templateUsesEnvconsul(t) {
			// The legacy chain uses the workflow identity, whose policy only carries
			// wildcard grants. Its configuration remains checked at release admission.
			for _, path := range paths {
				declare("*", path)
			}
		}
		if _, ok := t.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; ok {
			if tokenDisabled(t) || (wf.Spec.AutomountServiceAccountToken != nil && !*wf.Spec.AutomountServiceAccountToken) {
				add(fmt.Errorf("release WIF step %q disables its service-account token", t.Name))
			}
			// Reuse admission's leaf predicate without cloning the document.
			err := authorizeReleaseWIFLeaf(wf, t, top[t], config, request)
			if err != nil {
				add(fmt.Errorf("step %q: %w; update argo.releaseWIF for this repository/template/role", t.Name, err))
			}
		}
	})
	for _, path := range paths {
		if !consumed[path] {
			declare("*", path)
		}
	}
	add(authorizeWithApprovalTable(paths, request))
	add(admitSecretstoreExecPaths(wf, paths, request.StepGrants))
	// Workflow/default annotations are invalid even when no Pod leaf opted in.
	if _, err := authorizeReleaseWIF(wf, config, request); err != nil {
		add(err)
	}
	add(applyPerStepIdentities(wf, config, request))
	return result, nil
}

// PreflightReleaseAccounts performs GETs only. No TokenRequest or Workflow is
// created. Missing named-step identities are already reported by the static check.
func (controller *Controller) PreflightReleaseAccounts(ctx context.Context, request Request) error {
	config := controller.config
	if request.Identities != nil {
		config.PerRepoIdentities = request.Identities.Release
		config.PerRepoCIIdentities = request.Identities.CI
		config.PerStepIdentities = request.Identities.Step
	}
	wf, err := argoworkflow.Decode(request.Source)
	if err != nil {
		return err
	}
	if _, err = argoworkflow.Resolve(wf, request.Fragments); err != nil {
		return err
	}
	var problems []error
	problems = append(problems, applyPerStepIdentities(wf, config, request))
	wf.Namespace = config.Namespace
	checked := map[string]bool{}
	for _, template := range wf.Spec.Templates {
		sa := template.ServiceAccountName
		if sa == "" || checked[sa] {
			continue
		}
		checked[sa] = true
		one := &wfv1.Workflow{}
		one.Namespace = config.Namespace
		one.Spec.Templates = []wfv1.Template{template}
		problems = append(problems, controller.verifyPerStepServiceAccounts(ctx, one))
	}
	paths, pathErr := argoworkflow.DeclaredSecretPaths(wf)
	problems = append(problems, pathErr)
	wif := false
	walkTemplates(wf, func(t *wfv1.Template) {
		if _, ok := t.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; ok {
			wif = true
		}
	})
	if len(paths) > 0 || wif {
		identity, err := config.identityForWithRepo(periapsis.TriggerRelease, true, request.UpstreamName, request.UpstreamOrg, request.Repo)
		problems = append(problems, err)
		if err == nil {
			problems = append(problems, controller.verifyServiceAccount(ctx, config.Namespace, identity.ServiceAccountName))
		}
	}
	return errors.Join(problems...)
}
