package argoworkflow

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestResumeSecretStepFreshRunEquality(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Env = []core.EnvVar{{Name: "PRIVATE", ValueFrom: &core.EnvVarSource{SecretKeyRef: &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "private"}, Key: "value"}}}}
	request := requestFor(t, w)
	request.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "cluster-secret", "private")}
	old, err := Prepare(t.Context(), request, Policy{}, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	request.RunID = "r2"
	request.CommitSHA = strings.Repeat("f", 40)
	request.ObjectSHA = request.CommitSHA
	request.SourceReceipt.Revision.Commit = request.CommitSHA
	request.SourceReceipt.ObjectSHA = request.ObjectSHA
	fresh, err := Prepare(t.Context(), request, Policy{}, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if !SameExecution(old.View(), fresh.View()) || !fresh.View().Plan.Entries[0].SecretRoute {
		t.Fatal("fresh secret admission cannot resume")
	}
	for _, tc := range []string{"definition", "grant input", "revoked grant", "account", "pin", "plan"} {
		t.Run(tc, func(t *testing.T) {
			v := changedResumeView(t, tc, request, fresh.View())
			if SameExecution(old.View(), v) {
				t.Fatal("resume widened fresh admission", tc)
			}
		})
	}
	copy := old.View()
	copy.RunID = "new"
	copy.CommitSHA = strings.Repeat("f", 40)
	if !SameExecution(old.View(), copy) {
		t.Fatal("commit identity alone changed execution")
	}
}

func changedResumeView(t *testing.T, scenario string, request admissioncontract.PrepareRequest, v admissioncontract.PreparedView) admissioncontract.PreparedView {
	t.Helper()
	switch scenario {
	case "definition":
		v.Workflow.Spec.Templates[0].Container.Command = []string{"false"}
	case "grant input":
		v.Grants[0].Path = "other"
	case "revoked grant":
		r := request.Clone()
		r.Grants = nil
		if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err == nil {
			t.Fatal("fresh run reached a revoked secret")
		}
		v.Grants = nil
	case "account":
		v.Identity.Pipeline.UID = "replacement"
	case "pin":
		v.SourceBindings = append(v.SourceBindings, sourcecontract.Binding{Artifact: "changed pin"})
	case "plan":
		v.Plan.Entries[0].HoldArmed = true
	}
	return v
}
