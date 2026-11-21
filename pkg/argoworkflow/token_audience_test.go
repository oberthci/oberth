package argoworkflow

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestIdentityGrantRequiresANonEmptyAudience(t *testing.T) {
	t.Parallel()
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeSubmit, admissioncontract.ModeInspect} {
		for _, audience := range []string{"", "https://kubernetes.default.svc", "cluster-api", "login", "https://identity.example.test"} {
			t.Run(string(mode)+"/"+audience, func(t *testing.T) {
				w, p := tokenWorkflow()
				p.LoginAudience = "secret-store-only"
				w.Spec.Templates[0].Inputs.Artifacts = nil
				w.Spec.Templates[0].Container.WorkingDir = ""
				w.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = audience
				w.Spec.Templates[0].Container.VolumeMounts[0].MountPath = "/run/identity"
				p.Grants[0].Audience = audience
				r := requestFor(t, w)
				r.Grants, r.Mode, r.IdentityRequest.Mode = p.Grants, mode, mode
				_, err := prepareFixture(t.Context(), r, p, identityHooks())
				if audience == "" {
					requireAudienceRefusal(t, err)
				} else if err != nil {
					t.Fatal("granted explicit audience refused", err)
				}
			})
		}
	}
}

func TestExplicitAPIAudienceRequiresItsExactGrant(t *testing.T) {
	t.Parallel()
	w, p := tokenWorkflow()
	audience := "https://kubernetes.default.svc"
	w.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = audience
	p.Grants[0].Audience = audience
	if err := admitFixture(t.Context(), w, p); err != nil {
		t.Fatal("administrator's explicit audience grant refused", err)
	}
	p.Grants = nil
	requireCode(t, admitFixture(t.Context(), w, p), "grant")
}

func requireAudienceRefusal(t *testing.T, err error) {
	t.Helper()
	requireCode(t, err, "audience")
	for _, f := range admissionFindings(t, err) {
		if f.Code == "audience" && (len(f.Reason) > 80 || len(f.Remedy) > 120 || f.Reason == "" || f.Remedy == "") {
			t.Fatal("audience refusal must have a bounded reason and remedy", f)
		}
	}
}
