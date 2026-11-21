package argoworkflow

import (
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// SecretPathsAnnotation on a template lists the store paths that step reads.
// A Workflow-level copy is refused: each step declares its own paths.
const SecretPathsAnnotation = "oberth.ci/secret-paths"

// declaredSecretPaths returns the template's own list, or nil when it declares none.
func declaredSecretPaths(t *wfv1.Template) []string {
	declared := strings.TrimSpace(t.Metadata.Annotations[SecretPathsAnnotation])
	if declared == "" {
		return nil
	}
	paths := strings.Split(declared, ",")
	for i := range paths {
		paths[i] = strings.TrimSpace(paths[i])
	}
	return paths
}

// secretDeclaration refuses each malformed or repeated path once, as a path
// fault, and returns the paths that need a grant.
func (g *graph) secretDeclaration(n *node, paths []string) []string {
	where := n.path + ".metadata.annotations[" + SecretPathsAnnotation + "]"
	if len(paths) > admissioncontract.MaxSecretPaths {
		g.refuse(where, "secret_count")
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, value := range paths {
		if seen[value] || !workflowpolicy.ValidSecretPath(value) {
			g.refuse(where+"["+value+"]", "secret_path")
		} else {
			clean = append(clean, value)
		}
		seen[value] = true
	}
	return clean
}

// Admission selects one store account per template and only the paths that
// template declares: a step receives the intersection of its declaration and
// its grants. A grant the commit does not declare is not delivered and refuses
// nothing, so one grant set serves every branch of the file.
func (g *graph) selectSecretGrants() {
	g.secretGrants = map[string][]admissioncontract.GrantEvidence{}
	for _, n := range g.nodes {
		declared := declaredSecretPaths(n.template)
		var exact, wildcard []admissioncontract.GrantEvidence
		for _, grant := range g.request.Grants {
			if !g.grantApplies(n, grant) || !evidenceMatches(grant, "secret", grant.Path, "") || !slices.Contains(declared, grant.Path) {
				continue
			}
			if grant.Step == "*" {
				wildcard = append(wildcard, grant)
			} else {
				exact = append(exact, grant)
			}
		}
		if len(exact) == 0 {
			exact = wildcard
		}
		if len(exact) > 0 {
			g.secretGrants[n.key] = exact
		}
	}
}
