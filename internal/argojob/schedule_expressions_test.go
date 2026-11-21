package argojob_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	nativeTemplate "github.com/argoproj/argo-workflows/v4/util/template"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestScheduleRefusesExpressionsOutsideTemplates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, old, replacement, path string }{
		{"pod-name", "annotations: {note: pod}", "annotations: {run-note: '{{workflow.name}}'}", "spec.podMetadata.annotations.run-note"},
		{"workflow-name", "annotations: {note: workflow}", "annotations: {run-note: '{{workflow.name}}'}", "spec.workflowMetadata.annotations.run-note"},
		{"argument-name", "  entrypoint: build", "  entrypoint: build\n  arguments:\n    parameters: [{name: x, value: '{{workflow.name}}'}]", "spec.arguments.parameters[0].value"},
		{"pod-namespace", "annotations: {note: pod}", "annotations: {run-note: '{{workflow.namespace}}'}", "spec.podMetadata.annotations.run-note"},
		{"unknown-expression", "annotations: {note: pod}", "annotations: {run-note: '{{unknown}}'}", "spec.podMetadata.annotations.run-note"},
		{"unfinished-expression", "annotations: {note: pod}", "annotations: {run-note: 'literal{{'}", "spec.podMetadata.annotations.run-note"},
		{"map-key", "  entrypoint: build", "  entrypoint: build\n  nodeSelector: {'{{workflow.name}}': literal}", "spec.nodeSelector.{{workflow.name}}"},
		{"template-defaults", "  entrypoint: build", "  entrypoint: build\n  templateDefaults:\n    container:\n      env: [{name: RUN_NOTE, value: '{{workflow.name}}'}]", "spec.templateDefaults.container.env[0].value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := unpublishedScheduleFixture(t, strings.Replace(scheduleWorkflow, tc.old, tc.replacement, 1))
			admit := f.config.Admit
			var refusal error
			f.config.Admit = func(ctx context.Context, r cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
				p, err := admit(ctx, r)
				refusal = err
				return p, err
			}
			var err error
			f.syncer, err = cronsync.New(f.config)
			compositionOK(t, err)
			compositionOK(t, f.syncer.SyncAll(t.Context()))
			assertScheduleExpressionRefusal(t, refusal, tc.path)
			assertScheduleStatusField(t, f, tc.path)
			if f.prepared != nil || len(f.native.Actions()) != 0 {
				t.Fatal("refused schedule was prepared or published", f.native.Actions())
			}
			runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 10})
			compositionOK(t, err)
			if len(runs) != 0 {
				t.Fatal("refused schedule created a run")
			}
		})
	}
}

func TestScheduleAllowsRootMetadataExpressions(t *testing.T) {
	t.Parallel()
	document := strings.Replace(scheduleWorkflow, "  labels: {pipeline: authored}", "  labels: {pipeline: '{{workflow.name}}'}\n  annotations: {note: '{{workflow.namespace}}'}", 1)
	f := realScheduleDocument(t, document)
	retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
	compositionOK(t, err)
	if retained.Workflow.Labels["pipeline"] != "{{workflow.name}}" || retained.Workflow.Annotations["note"] != "{{workflow.namespace}}" {
		t.Fatal("preparation changed root metadata expressions")
	}
	f.child = initializeScheduleChild(t, f.child)
	for range 2 {
		compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	}
	assertScheduleRecord(t, f, retained)
}

func assertScheduleStatusField(t *testing.T, f *childFixture, path string) {
	t.Helper()
	repositories, err := f.config.Repositories(t.Context())
	compositionOK(t, err)
	status, err := f.syncer.Status(t.Context(), repositories[0], "nightly")
	compositionOK(t, err)
	if !status.SyncSuspended || len(status.Error) > 40 || !strings.Contains(status.Detail, path) {
		t.Fatal("schedule status lost the refused field", status)
	}
}

func assertScheduleExpressionRefusal(t *testing.T, err error, path string) {
	t.Helper()
	var refusal *app.Refusal
	if !errors.As(err, &refusal) || !publicerr.Permanent(err) || len(refusal.Problems) != 1 {
		t.Fatal("schedule did not refuse its expression", err)
	}
	f := refusal.Problems[0]
	if f.Path != path || len(refusal.Reason()) > 40 || len(f.Path+": "+f.Reason) > 80 || len(f.Remedy) > 120 || !strings.Contains(f.Remedy, "template") || !strings.Contains(f.Remedy, "literal") {
		t.Fatal("schedule refusal lost its field or bounded remedy", refusal)
	}
}

func TestScheduleTemplateExpressionsRecordInitializedChild(t *testing.T) {
	t.Parallel()
	for _, expression := range []string{"{{workflow.name}}", "{{workflow.scheduledTime}}", "{{workflow.parameters.x}}"} {
		t.Run(expression, func(t *testing.T) {
			document := strings.Replace(scheduleWorkflow, "  entrypoint: build", "  entrypoint: build\n  arguments:\n    parameters: [{name: x, value: literal}]", 1)
			document = strings.Replace(document, `command: ["true"]`, `command: ["echo", "`+expression+`"]`, 1)
			f := realScheduleDocument(t, document)
			retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
			compositionOK(t, err)
			f.child = initializeScheduleChild(t, f.child)
			for range 2 {
				compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			}
			assertScheduleRecord(t, f, retained)
			if f.child.Spec.Templates[0].Container.Command[1] != expression {
				t.Fatal("initialization changed a template-local expression")
			}
		})
	}
}

// Every real admitted schedule fixture checks the pinned substitution boundary,
// including the product's stamps and the populated-policy fixture.
func assertScheduleSpecUnchanged(t *testing.T, workflow *wf.Workflow) {
	t.Helper()
	spec := workflow.Spec.DeepCopy()
	spec.Templates = nil
	raw, err := json.Marshal(spec)
	compositionOK(t, err)
	resolved, err := nativeTemplate.Replace(t.Context(), string(raw), map[string]any{
		"workflow.name": "initialized-child", "workflow.namespace": "ci", "workflow.scheduledTime": "2026-11-18T03:00:00Z", "workflow.parameters.x": "literal",
	}, true)
	compositionOK(t, err)
	var observed wf.WorkflowSpec
	compositionOK(t, json.Unmarshal([]byte(resolved), &observed))
	if !reflect.DeepEqual(*spec, observed) || strings.Contains(string(raw), "{{") {
		t.Fatal("admitted schedule changes during native spec substitution")
	}
}
