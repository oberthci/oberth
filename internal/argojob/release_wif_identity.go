package argojob

import (
	"fmt"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Validate against the current identity snapshot too: a later grant reconcile
// must not alias a pinned WIF-only account to a repository or another step.
func (config Config) validateReleaseWIFStepAccounts() error {
	if config.ReleaseWIF == nil {
		return nil
	}
	for repo, grant := range config.ReleaseWIF.Repositories {
		for leaf, sa := range grant.StepServiceAccounts {
			if !slices.Contains(config.ReleaseTagRoutes[repo], grant.StepRoutes[leaf]) {
				return fmt.Errorf("argojob: release WIF step %q must bind an existing exact release tag route", leaf)
			}
			if sa == config.PipelineServiceAccount || sa == config.CredentialedServiceAccount || sa == config.CISecretsServiceAccount || sa == config.ExecutorServiceAccount || sa == config.VMLauncherServiceAccount {
				return fmt.Errorf("argojob: release WIF step %q cannot use shared ServiceAccount %q", leaf, sa)
			}
			for _, identities := range []map[string]PerRepoIdentityConfig{config.PerRepoIdentities, config.PerRepoCIIdentities} {
				for _, identity := range identities {
					if identity.ServiceAccountName == sa {
						return fmt.Errorf("argojob: release WIF step %q cannot reuse a repository ServiceAccount", leaf)
					}
				}
			}
			for key, identity := range config.PerStepIdentities {
				if key != repo+"/"+leaf && identity.ServiceAccountName == sa {
					return fmt.Errorf("argojob: release WIF step %q cannot reuse another step's ServiceAccount", leaf)
				}
			}
		}
	}
	return nil
}

// Only leaves already authorized by the exact WIF capability predicate enter
// this pass. It adds neither secret grants nor a Vault role/token.
func applyReleaseWIFStepIdentities(leaves []*wfv1.Template, config Config, request Request) error {
	if len(leaves) == 0 {
		return nil
	}
	if config.ReleaseWIF == nil || request.Trigger != periapsis.TriggerRelease || request.effectiveRunTrigger() != argoworkflow.RunTriggerTag {
		return fmt.Errorf("argojob: WIF step identities require a tag release")
	}
	if err := config.ReleaseWIF.Validate(); err != nil {
		return err
	}
	if err := config.validateReleaseWIFStepAccounts(); err != nil {
		return err
	}
	grant := config.ReleaseWIF.Repositories[canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo)]
	for _, leaf := range leaves {
		sa, mapped := grant.StepServiceAccounts[leaf.Name]
		if !mapped {
			continue
		} // Existing repository/named-grant identities unchanged.
		if !config.releaseWIFStepRouteMatches(canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo), leaf.Name, request) {
			return fmt.Errorf("argojob: WIF-only identity for template %q is not approved for this route", leaf.Name)
		}
		if templateUsesOberthSecretstore(leaf) || templateUsesEnvconsul(leaf) {
			return fmt.Errorf("argojob: pinned WIF-only identity for template %q cannot consume the secret store", leaf.Name)
		}
		leaf.ServiceAccountName = sa
		templateContainers(leaf, func(c *corev1.Container) {
			env := c.Env[:0:0]
			for _, value := range c.Env {
				if !strings.HasPrefix(value.Name, "VAULT_") && value.Name != "OBERTH_VAULT_ROLE" {
					env = append(env, value)
				}
			}
			c.Env = env
		})
	}
	return nil
}

func (controller *Controller) isReleaseWIFStepServiceAccount(sa string) bool {
	if controller.config.ReleaseWIF != nil {
		for _, repo := range controller.config.ReleaseWIF.Repositories {
			for _, mapped := range repo.StepServiceAccounts {
				if sa == mapped {
					return true
				}
			}
		}
	}
	return false
}

// Pinned WIF-only accounts are independent of secret-derived repository SAs.
// Only legacy WIF leaves without a dedicated mapping need the repository SA.
func releaseWIFNeedsRepoIdentity(leaves []*wfv1.Template, config Config, request Request) bool {
	if config.ReleaseWIF == nil {
		return false
	}
	grant := config.ReleaseWIF.Repositories[canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo)]
	for _, leaf := range leaves {
		if grant.StepServiceAccounts[leaf.Name] == "" {
			return true
		}
	}
	return false
}

// Preflight may inspect a file's planned identity without inventing a tag. This
// private mode never reaches Build; actual delivery must resolve the exact
// configured pattern AND file from the admitted tag, and read that same file.
func (config Config) releaseWIFStepRouteMatches(repo, leaf string, request Request) bool {
	grant := config.ReleaseWIF.Repositories[repo]
	pinned, ok := grant.StepRoutes[leaf]
	if !ok || !slices.Contains(config.ReleaseTagRoutes[repo], pinned) {
		return false
	}
	file := request.PipelineFile
	if file == "" {
		file = argoworkflow.ReleaseFile
	}
	if file != pinned.File {
		return false
	}
	if request.releasePreflight {
		return true
	}
	if request.effectiveRunTrigger() != argoworkflow.RunTriggerTag {
		return false
	}
	selected, err := config.ReleaseTagRoutes.resolveRoute(repo, model.RefTag, request.Ref)
	return err == nil && selected == pinned
}
