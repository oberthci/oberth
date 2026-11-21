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
	verify := argojob.New(argojob.Config{Accounts: fake.Accounts{Names: names}}).Hooks(nativebinding.Preparation{}).VerifyIdentities
	request := admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: "pipeline", ResourceAccount: "resources"}
	evidence, err := verify(t.Context(), request)
	if err != nil || evidence.Pipeline.UID != "pipeline-uid" || evidence.Resource == nil || *evidence.Resource != (admissioncontract.AccountEvidence{Name: "resources", UID: "resource-uid"}) {
		t.Fatal("live resource identity was not verified", evidence, err)
	}
	delete(names, "resources")
	if _, err := verify(t.Context(), request); err == nil {
		t.Fatal("missing resource account accepted")
	}
}
