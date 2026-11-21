package argojob_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestInspectionReportsMissingAccountsWithoutProvisioning(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"pipeline", "resource"} {
		t.Run(missing, func(t *testing.T) {
			names := map[string]string{"pipeline": "pipeline-uid", "resource": "resource-uid"}
			delete(names, missing)
			e := argojob.New(argojob.Config{Accounts: fake.Accounts{Names: names}})
			hooks := e.Hooks(nativebinding.Preparation{})
			q := admissioncontract.IdentityRequest{Mode: admissioncontract.ModeInspect, Namespace: "runs", PipelineAccount: "pipeline", ResourceAccount: "resource"}
			evidence, err := hooks.VerifyIdentities(t.Context(), q)
			if err != nil || evidence.State != admissioncontract.StateLiveRequired || evidence.Resource == nil {
				t.Fatal("missing account was not deferred", evidence, err)
			}
			if evidence.Pipeline.UID != names["pipeline"] || evidence.Resource.UID != names["resource"] {
				t.Fatal("inspection invented account evidence", evidence)
			}
			q.Mode = admissioncontract.ModeSubmit
			if _, err := hooks.VerifyIdentities(t.Context(), q); err == nil {
				t.Fatal("submission accepted the missing account")
			}
		})
	}
}
