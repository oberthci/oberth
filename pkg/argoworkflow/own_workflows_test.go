package argoworkflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	yaml "go.yaml.in/yaml/v3"
)

func TestOwnWorkflowsSubmitAdmission(t *testing.T) {
	t.Parallel()
	for _, file := range []string{workflowpolicy.BuildFile, workflowpolicy.ReleaseFile} {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../..", file))
			if err != nil {
				t.Fatal(err)
			}
			w, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			r := documentationDocument(requestFor(t, w), raw)
			r.SelectedFile = file
			r.SourceReceipt.File = r.SelectedFile
			hooks := identityHooks()
			hooks.ReleaseFacts = func(context.Context) (admissioncontract.ReleaseFacts, error) {
				return admissioncontract.ReleaseFacts{Version: "v0.1.0", TestedCommit: r.CommitSHA, DefaultCommit: r.CommitSHA, BuildStatus: "passed"}, nil
			}
			p := Policy{ServerNamespace: "oberth-system", ArgoNamespace: "ci", PVCClaims: map[string]bool{"build-cache": true, "release-cache": true}}
			if file == workflowpolicy.BuildFile {
				// The build gets what any repository's would: no capability or resource account.
				hooks.LoginAudience = ownChartAudience(t)
				r.Grants = ownPolicyGrants(r)
			} else {
				r.Repository = "github/oberthci/oberth"
				r.SourceReceipt.Revision.Repository = r.Repository
				r.Ref, r.Trigger = "refs/tags/v0.1.0", "tag"
				p.Release = true
				hooks.LoginAudience = ownChartAudience(t)
				r.Grants = ownReleaseGrants(r)
			}
			prepared, err := prepareDocumentation(t, r, p, hooks)
			if err != nil || prepared == nil {
				t.Fatalf("own workflow not admitted in submit mode: %v", err)
			}
			if prepared.View().SourceReceipt.DocumentDigest != r.SourceReceipt.DocumentDigest {
				t.Fatal("authored workflow changed")
			}
		})
	}
}

func ownReleaseGrants(r admissioncontract.PrepareRequest) []admissioncontract.GrantEvidence {
	grants := []admissioncontract.GrantEvidence{}
	for _, entry := range []struct{ step, kind, path, audience string }{
		{"*", "configmap", "release-config", ""}, {"*", "secret", "oberth/data/release/signing", ""},
		{"release-images-pushed", "identity", "identity", "oberth-release-registry"},
		{"*", "secret", "oberth/data/release/github-token", ""},
	} {
		grants = append(grants, admissioncontract.GrantEvidence{Repository: r.Repository, File: r.SelectedFile, Step: entry.step, Kind: entry.kind, Path: entry.path, Audience: entry.audience, Revision: "operator-approval", State: admissioncontract.StateVerified})
	}
	return grants
}

func ownChartAudience(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../charts/oberth/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		SecretStore struct{ Audience string } `yaml:"secretStore"`
	}
	if err := yaml.Unmarshal(body, &values); err != nil || values.SecretStore.Audience == "" {
		t.Fatal("chart login audience missing", err)
	}
	return values.SecretStore.Audience
}

func ownPolicyGrants(r admissioncontract.PrepareRequest) []admissioncontract.GrantEvidence {
	return []admissioncontract.GrantEvidence{
		{Repository: r.Repository, File: r.SelectedFile, Step: "lint-and-guards-pass", Kind: "secret", Path: "oberth/policy/names", Revision: "operator-approval", State: admissioncontract.StateVerified},
		{Repository: r.Repository, File: r.SelectedFile, Step: "lint-and-guards-pass", Kind: "secret", Path: "oberth/policy/identity", Revision: "operator-approval", State: admissioncontract.StateVerified},
	}
}

func TestPipelineResourcesRequireNoStoreAudienceGrant(t *testing.T) {
	raw, err := os.ReadFile("../../.oberth/pipeline-resources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "--audience oberth-store") || !strings.Contains(string(raw), "oberth secret put --repo") {
		t.Fatal("store setup instructions differ")
	}
}
