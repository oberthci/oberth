package argojob_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func selectedGrant() admissioncontract.GrantEvidence {
	return admissioncontract.GrantEvidence{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: "private", Kind: "secret", Path: "public/key", Revision: "1", State: admissioncontract.StateVerified}
}

type secretPreparation struct {
	policy argoworkflow.Policy
	image  string
}

func prepareSelectedSecrets(t *testing.T, w *wf.Workflow, grants []admissioncontract.GrantEvidence, policy argoworkflow.Policy) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	return prepareSelectedSecretImage(t, w, grants, secretPreparation{policy, secretHelperTestImage})
}

func prepareSelectedSecretImage(t *testing.T, w *wf.Workflow, grants []admissioncontract.GrantEvidence, options secretPreparation) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	document, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sha, blob := strings.Repeat("a", 40), strings.Repeat("b", 40)
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: "forge/org/repo", RunID: "run-0000000000000001", Ref: "refs/heads/topic", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", FileBlob: blob, Document: document, Grants: grants,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: "forge/org/repo", Commit: sha}, ObjectSHA: sha, File: ".oberth/build.yaml", Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: account(t, identityname.PipelineCI, "")}}
	accounts := map[string]string{}
	for _, step := range []string{"", "private", "*"} {
		kind := identityname.StepCI
		if step == "" {
			kind = identityname.PipelineCI
		}
		accounts[account(t, kind, step)] = "uid"
	}
	if capability := options.policy.Capability; capability != nil {
		request.IdentityRequest.ResourceAccount = capability.ServiceAccount
		accounts[capability.ServiceAccount] = "resource-uid"
	}
	executor := argojob.New(argojob.Config{SecretStore: secretDelivery(), Accounts: fake.Accounts{Names: accounts}})
	return argoworkflow.Prepare(t.Context(), request, options.policy, executor.Hooks(nativebinding.Preparation{Repository: request.Repository, File: request.SelectedFile, Assist: true, HoldTTL: time.Minute, HelperImage: options.image}))
}

func secretSelectionWorkflow(kind string, declared bool) *wf.Workflow {
	w := secretWorkflow()
	w.Spec.Templates[1] = ordinarySecretLeaf(kind)
	if !declared {
		return w
	}
	command := []string{"oberth", "secretstore", "exec", "--", "true"}
	leaf := &w.Spec.Templates[1]
	leaf.Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "public/key"}
	switch kind {
	case "container":
		leaf.Container.Command = command
	case "script":
		leaf.Script.Command = command
	case "container-set":
		leaf.ContainerSet.Containers[0].Command = command
	}
	return w
}

func TestAdmissionSecretEvidence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		for _, field := range []string{"state", "revision", "audience"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				checkAdmissionSecretEvidence(t, kind, field)
			})
		}
	}
}

func checkAdmissionSecretEvidence(t *testing.T, kind, field string) {
	t.Helper()
	grant := selectedGrant()
	switch field {
	case "state":
		grant.State = admissioncontract.StateUnknown
	case "revision":
		grant.Revision = ""
	case "audience":
		grant.Audience = "other-audience"
	}
	prepared, err := prepareSelectedSecrets(t, secretSelectionWorkflow(kind, false), []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	view := prepared.View()
	leaf := view.Workflow.Spec.Templates[1]
	assertNoStoreInputs(t, leaf)
	if leaf.ServiceAccountName != account(t, identityname.PipelineCI, "") {
		t.Fatal("unverified evidence selected a store account")
	}
	for _, entry := range view.Plan.Entries {
		if entry.CredentialIntent {
			t.Fatal("unverified evidence became credential intent")
		}
	}
	if _, err = prepareSelectedSecrets(t, secretSelectionWorkflow(kind, true), []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{}); err == nil {
		t.Fatal("unverified declared secret admitted")
	}
}

func TestAdmissionMixedSecretGrants(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		t.Run(kind, func(t *testing.T) {
			checkAdmissionMixedSecretGrants(t, kind)
		})
	}
}

func checkAdmissionMixedSecretGrants(t *testing.T, kind string) {
	t.Helper()
	w := secretSelectionWorkflow(kind, true)
	w.Spec.Templates[1].Metadata.Annotations[argoworkflow.SecretPathsAnnotation] = "public/key,shared/key"
	exact, wildcard := selectedGrant(), selectedGrant()
	wildcard.Step, wildcard.Path = "*", "shared/key"
	grants := []admissioncontract.GrantEvidence{wildcard, exact}
	_, err := prepareSelectedSecrets(t, w, grants, argoworkflow.Policy{})
	var admission *argoworkflow.AdmissionError
	if !errors.As(err, &admission) {
		t.Fatal("mixed-account declaration was not refused", err)
	}
	want := "oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'private' 'shared/key' < <value-file>"
	found := false
	for _, finding := range admission.Findings {
		if finding.Command == want {
			found = true
		}
	}
	if !found {
		t.Fatal("missing exact-step grant remedy", err)
	}
	exact.Path = "shared/key"
	grants = append(grants, exact)
	prepared, err := prepareSelectedSecrets(t, w, grants, argoworkflow.Policy{})
	if err != nil {
		t.Fatal("exact grant did not repair declaration", err)
	}
	view := prepared.View()
	leaf := view.Workflow.Spec.Templates[1]
	if leaf.ServiceAccountName != account(t, identityname.StepCI, "private") {
		t.Fatal("exact account lost")
	}
	for _, container := range secretLeafContainers(leaf) {
		_, paths := secretEnvironment(t, container.Env)
		if len(paths) != 2 || paths[0] != "public/key" || paths[1] != "shared/key" {
			t.Fatal("selected account paths differ")
		}
	}
	assertGrantedPlan(t, view.Plan)
}

func TestAdmissionGrantSelectionScopesEveryFact(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"repository", "file", "step", "kind"} {
		t.Run(field, func(t *testing.T) {
			grant := selectedGrant()
			switch field {
			case "repository":
				grant.Repository = "forge/other/repo"
			case "file":
				grant.File = ".oberth/other.yaml"
			case "step":
				grant.Step = "other"
			case "kind":
				grant.Kind = "identity"
			}
			prepared, err := prepareSelectedSecrets(t, secretSelectionWorkflow("container", false), []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			assertNoStoreInputs(t, prepared.View().Workflow.Spec.Templates[1])
		})
	}
}

func TestNoSecretGrantsKeepSealedSpec(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		t.Run(kind, func(t *testing.T) {
			w := secretSelectionWorkflow(kind, false)
			first, err := prepareSelectedSecrets(t, w, nil, argoworkflow.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			grant := selectedGrant()
			grant.Repository = "forge/other/repo"
			second, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			if first.View().SpecDigest != second.View().SpecDigest {
				t.Fatal("irrelevant grants changed the sealed spec")
			}
			t.Log("sealed-spec", kind, first.View().SpecDigest)
		})
	}
}
