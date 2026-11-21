package cronsync_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func requestFixture(t *testing.T, f *fixture) cronsync.Preparation {
	t.Helper()
	file, err := schedule.Parse([]byte("schedules: [{name: nightly, cron: '0 3 * * *'}]"), schedule.Limits{}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return cronsync.Preparation{Repository: f.repo, Entry: file.Entries[0], Revision: gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)}}
}
func TestScheduledSourceRefusalNamesEntryAndStep(t *testing.T) {
	f := newFixture(t)
	request := requestFixture(t, f)
	view := prepared(request)
	view.SourceBindings = []sourcecontract.Binding{{Path: "main"}}
	sealed, err := admissioncontract.Seal(view)
	if err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.Admit = func(context.Context, cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		return sealed, nil
	}
	f.c, err = cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sync()
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	want := "scheduled source is not supported - use a push or manual schedule trigger\nentry \"nightly\": field \"source\" has value step \"main\": use a push or manual schedule trigger."
	if err != nil || status.Error != "schedule file is invalid" || status.Detail != want || len(f.native.Actions()) != 0 {
		t.Fatal(status, err)
	}

}
func TestSealedSpecAndCapturedTemplatesStayUnchangedAcrossValidation(t *testing.T) {
	f := newFixture(t)
	request := requestFixture(t, f)
	view := prepared(request)
	view.Templates = []admissioncontract.TemplateSnapshot{{Name: "captured", Namespace: "ci", UID: "template-uid", ResourceVersion: "1", Spec: []byte(`{"entrypoint":"main"}`)}}
	sealed, err := admissioncontract.Seal(view)
	if err != nil {
		t.Fatal(err)
	}
	before := sealed.View()
	config := f.config
	config.Admit = func(context.Context, cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		return sealed, nil
	}
	validated := false
	config.ValidateNativeSchema = func(_ context.Context, object *wfv1.CronWorkflow, templates []admissioncontract.TemplateSnapshot) error {
		validated = true
		if !reflect.DeepEqual(object.Spec.WorkflowSpec, before.Workflow.Spec) || !reflect.DeepEqual(templates, before.Templates) {
			t.Fatal("validation lost sealed spec/evidence")
		}
		object.Spec.WorkflowSpec.Entrypoint = "changed"
		templates[0].Spec[0] = 'x'
		return nil
	}
	f.c, err = cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sync()
	if !validated || !reflect.DeepEqual(f.object(f.repo, "nightly").Spec.WorkflowSpec, before.Workflow.Spec) || !reflect.DeepEqual(sealed.View(), before) {
		t.Fatal("validation or native apply changed sealed admission")
	}
}
