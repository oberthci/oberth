//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A source-consuming schedule is admitted; the child Argo creates from it is
// recorded on the commit the parent sealed, its source roots are kept under
// its run ID, and the real plugin, using the locator the child's spec carries,
// unpacks exactly that commit from the child's own Pod.
func TestComposedScheduledChildLoadsItsRecordedSource(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledDocument(t, composedWorkflow)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	locator := workflowParameter(parent.Spec.WorkflowSpec, "oberth-source")
	if locator != sourcecontract.ScheduledLocator {
		t.Fatal("schedule parent does not carry the scheduled locator", locator)
	}
	ctx := argologging.WithLogger(t.Context(), scheduledFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-789", f.clock.Now())
	child.Namespace = f.config.PipelineNamespace
	child = initializedScheduledObservation(t, child, f.clock.Now())
	child, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Create(t.Context(), child, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.ID == child.Name && r.Origin.WorkflowUID == string(child.UID) })
	if run.CommitSHA == nil || *run.CommitSHA != commit || len(run.Admission.SourceBindings) != 1 || run.Admission.SourceBindings[0].Revision.Commit != commit {
		t.Fatal("scheduled run is not bound to the admitted commit", run.RunSummary, run.Admission.SourceBindings)
	}
	// The acceptance's own Keep is checked in the native package; here the
	// cache's collection pass must retain the child's commit as a live owner.
	f.checkRetention(t, run)
	path := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	if refs := f.git.Git(t, path, "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"+run.ID+"/"); !strings.Contains(refs, "refs/oberth/keep/"+run.ID+"/"+commit) {
		t.Fatal("scheduled run's source is not kept", refs)
	}
	f.deliverLocated(t, run, locator, nil)
}

func workflowParameter(spec wf.WorkflowSpec, name string) string {
	for _, p := range spec.Arguments.Parameters {
		if p.Name == name && p.Value != nil {
			return p.Value.String()
		}
	}
	return ""
}
