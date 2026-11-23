package argoworkflow

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	core "k8s.io/api/core/v1"
)

func TestOfflineAuthorizationNeedsAreUnverified(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"secret", "audience", "capability", "pvc", "read", "device"} {
		t.Run(kind, func(t *testing.T) {
			r, policy, code := offlineAuthorityFixture(t, kind)
			r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
			prepared, err := prepareFixture(t.Context(), r, policy, admissioncontract.PrepareHooks{OfflineValidation: true})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range prepared.View().Findings {
				if f.Code == code {
					found = true
					assertUnverifiedAuthority(t, f)
				}
				if f.Code == "live" {
					assertUnverifiedAuthority(t, f)
				}
			}
			if !found {
				t.Fatal("authorization need missing", code, prepared.View().Findings)
			}
		})
	}
}

func assertUnverifiedAuthority(t *testing.T, f admissioncontract.Finding) {
	t.Helper()
	if !strings.Contains(f.Reason, "unverified") && !strings.Contains(f.Reason, "offline") || !strings.Contains(f.Remedy, "oberth check") {
		t.Fatal("unchecked evidence claimed missing authority", f)
	}
	if publicerr.CheckMessage(f.Reason, 40) != nil || publicerr.CheckMessage(f.Remedy, 120) != nil {
		t.Fatal("offline message exceeds its budget", f)
	}
}

func offlineAuthorityFixture(t *testing.T, kind string) (admissioncontract.PrepareRequest, admissionFixture, string) {
	t.Helper()
	w := simpleWorkflow()
	policy, code := submitPolicy(), kind
	switch kind {
	case "secret":
		return secretSelectionRequest(t, "public/key"), policy, "grant"
	case "audience":
		w, _ = tokenWorkflow()
		policy.LoginAudience, code = "login", "grant"
	case "capability":
		w, policy = resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: deploy, namespace: resources}")
		policy.Capability = nil
	case "pvc":
		w.Spec.Volumes = []core.Volume{{Name: "cache", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "cache"}}}}
	case "read":
		w.Spec.Templates[0].Container.EnvFrom = []core.EnvFromSource{{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "private"}}}}
	case "device":
		w.Spec.Templates[0].Container.Resources = deviceResources("limits")
	}
	return requestFor(t, w), policy, code
}

func TestOfflineWordingPreservesSubmissionRefusals(t *testing.T) {
	t.Parallel()
	r := secretSelectionRequest(t, "public/key")
	_, err := prepareFixture(t.Context(), r, submitPolicy(), admissioncontract.PrepareHooks{OfflineValidation: true})
	for _, f := range admissionFindings(t, err) {
		if f.Code == "grant" {
			if !strings.Contains(f.Reason, "no exact operator grant") {
				t.Fatal("submission hid a missing grant", f)
			}
			return
		}
	}
	t.Fatal("submission did not refuse the missing grant", err)
}
