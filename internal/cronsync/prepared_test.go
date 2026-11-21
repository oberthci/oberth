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
// A source-consuming admission becomes an active parent with its sealed spec;
// when the entry's branch is gone the parent is suspended, its spec unchanged,
// and the entry shows the existing branch refusal.
func TestScheduledSourceParentIsAdmittedAndSuspendsWithoutBranch(t *testing.T) {
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
	admitted := f.object(f.repo, "nightly")
	if err != nil || status.Error != "" || admitted.Spec.Suspend || !reflect.DeepEqual(admitted.Spec.WorkflowSpec, sealed.View().Workflow.Spec) {
		t.Fatal("source schedule was not admitted", status, err)
	}
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *', branch: absent}]")
	f.sync()
	status, err = f.c.Status(t.Context(), f.repo, "nightly")
	suspended := f.object(f.repo, "nightly")
	if err != nil || status.Error != "schedule branch not found" || !suspended.Spec.Suspend || !reflect.DeepEqual(suspended.Spec.WorkflowSpec, admitted.Spec.WorkflowSpec) {
		t.Fatal("absent branch did not suspend the source schedule", status, err)
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
