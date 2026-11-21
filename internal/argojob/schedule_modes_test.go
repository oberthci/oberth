package argojob_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestNonParentHooksDoNotStampChildIdentity(t *testing.T) {
	t.Parallel()
	f := realScheduleFixture(t)
	v := f.prepared.View()
	for _, tc := range []struct {
		name    string
		mode    admissioncontract.Mode
		trigger model.RunTrigger
		runID   string
	}{
		{"manual-schedule", admissioncontract.ModeSubmit, model.TriggerSchedule, "manual"},
		{"branch-check", admissioncontract.ModeInspect, model.TriggerBranch, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := ""
			if tc.trigger == model.TriggerSchedule {
				entry = "nightly"
			}
			q := admissioncontract.PrepareRequest{Mode: tc.mode, Repository: v.Repository, RunID: tc.runID, Ref: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: v.CommitSHA, Trigger: tc.trigger, ScheduleEntry: entry, SelectedFile: v.SelectedFile, FileBlob: v.FileBlob, Document: []byte(scheduleWorkflow), SourceReceipt: v.SourceReceipt,
				IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: v.Identity.Pipeline.Name}}
			e := argojob.New(argojob.Config{Accounts: nativefake.Accounts{Names: map[string]string{v.Identity.Pipeline.Name: v.Identity.Pipeline.UID}}})
			preparation := nativebinding.Preparation{Repository: v.Repository, RepositoryID: f.configRepositoryID(t), File: v.SelectedFile, Trigger: string(tc.trigger), Ref: sourcecontract.Ref{Name: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: v.CommitSHA}}
			p, err := argoworkflow.Prepare(t.Context(), q, argoworkflow.Policy{}, e.Hooks(preparation))
			compositionOK(t, err)
			w := p.View().Workflow
			for _, parameter := range w.Spec.Arguments.Parameters {
				if parameter.Name == "oberth-ref" || parameter.Name == "oberth-sha" {
					t.Fatal("ordinary admission gained schedule parameters")
				}
			}
			for name := range scheduleProducedFacts(t, w) {
				if strings.Contains(name, "Metadata.") || strings.HasPrefix(name, "metadata.") {
					t.Fatal("ordinary admission stamped child identity", name)
				}
			}
		})
	}
}
