package argojob_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestVerifyIdentitiesRequiresLiveResourceAccount(t *testing.T) {
	t.Parallel()
	names := map[string]string{"pipeline": "pipeline-uid", "resources": "resource-uid"}
	executor := argojob.New(argojob.Config{Accounts: fake.Accounts{Names: names}})
	request := admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: "pipeline", ResourceAccount: "resources"}
	evidence, err := executor.Hooks(nativebinding.Preparation{}).VerifyIdentities(t.Context(), request)
	if err != nil || evidence.Pipeline.UID != "pipeline-uid" || evidence.Resource == nil || *evidence.Resource != (admissioncontract.AccountEvidence{Name: "resources", UID: "resource-uid"}) {
		t.Fatal("live resource identity was not verified", evidence, err)
	}
	delete(names, "resources")
	// Each admission takes its own hooks and so reads the cluster afresh.
	if _, err := executor.Hooks(nativebinding.Preparation{}).VerifyIdentities(t.Context(), request); err == nil {
		t.Fatal("missing resource account accepted")
	}
}
