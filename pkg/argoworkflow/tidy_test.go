package argoworkflow

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestFinalGraphReusesAuthoredSecretSelection(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	declareSecretPaths(&w.Spec.Templates[0], "public/key")
	request := requestFor(t, w)
	grant := admissioncontract.GrantEvidence{Repository: request.Repository, File: request.SelectedFile, Step: "main", Kind: "secret", Path: "public/key", Revision: "1", State: admissioncontract.StateVerified}
	request.Grants = []admissioncontract.GrantEvidence{grant}
	hooks := identityHooks()
	hooks.LoginAudience = "login"
	g := (Policy{}).admitRequest(t.Context(), w, request, hooks)
	if err := g.err(); err != nil {
		t.Fatal(err)
	}
	g.request.Grants = nil
	final := g.finalGraph(w)
	selected := final.secretGrants["main"]
	if len(selected) != 1 || selected[0] != grant {
		t.Fatal("final graph did not retain the authored selection")
	}
	if plan := final.plan(); len(plan.Entries) != 1 || !plan.Entries[0].CredentialIntent {
		t.Fatal("final plan lost credential intent")
	}
}

func TestAuthoredIdentityMountDoesNotDependOnStoreAudience(t *testing.T) {
	t.Parallel()
	for _, audience := range []string{"login", "other-identity"} {
		t.Run(audience, func(t *testing.T) {
			w, fixture := tokenWorkflow()
			fixture.LoginAudience = audience
			w.Spec.Templates[0].Container.VolumeMounts[0].MountPath = "/identity"
			w.Spec.Templates[0].Inputs.Artifacts = nil
			w.Spec.Templates[0].Container.WorkingDir = ""
			request := requestFor(t, w)
			request.Grants = fixture.Grants
			hooks := identityHooks()
			hooks.LoginAudience = fixture.LoginAudience
			err := fixture.admit(t.Context(), w, request, hooks)
			if err != nil {
				t.Fatal(err)
			}
			_, preparedErr := Prepare(t.Context(), request, fixture.Policy, hooks)
			if (err == nil) != (preparedErr == nil) {
				t.Fatal("admission and preparation disagree on the delivery audience", err, preparedErr)
			}
		})
	}
}
