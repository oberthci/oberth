package argoworkflow

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestOfflineAuthorizationCommandsInspectBeforeWriting(t *testing.T) {
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
				if f.Code != code && f.Code != "live" {
					continue
				}
				found = found || f.Code == code
				if f.Command != publicerr.CheckCommand(r.Repository, r.SelectedFile) {
					t.Fatal("unchecked authorization suggests a write", f)
				}
			}
			if !found {
				t.Fatal("authorization need missing", code)
			}
		})
	}
}
