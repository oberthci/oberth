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
	"github.com/oberthci/oberth/internal/stepmodel"
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
	child := f.fireScheduledChild(t, parent, "789")
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

// A child whose sealed commit is already gone fails with the reason a pushed
// run gets when its source cannot be kept, its Workflow is terminated, and
// the cache's collection pass still runs for everyone else.
func TestComposedScheduledChildWithGoneSourceFailsLikeAPush(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledDocument(t, composedWorkflow)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	path := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	orphan := strings.TrimSpace(f.git.Git(t, path, "-c", "user.name=probe", "-c", "user.email=probe@example.invalid", "commit-tree", commit+"^{tree}", "-m", "orphan"))
	for _, ref := range []string{"refs/heads/main", "refs/oberth/upstream/heads/main"} {
		f.git.Git(t, path, "update-ref", ref, orphan)
	}
	if err := f.owners.cache.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	child := f.fireScheduledChild(t, parent, "790")
	run := f.waitRun(t, func(r stateport.Run) bool {
		return r.ID == child.Name && r.Status == stepmodel.RunFailed
	})
	if run.DisplayReason != stateport.ReasonRetentionRefused {
		t.Fatal("gone source did not fail like a push", run.RunSummary)
	}
	native, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Get(t.Context(), child.Name, meta.GetOptions{})
	if err != nil || native.Spec.Shutdown != wf.ShutdownStrategyTerminate {
		t.Fatal("child Workflow was not terminated", err)
	}
	if err = f.owners.cache.GC(t.Context()); err != nil {
		t.Fatal("a gone child stopped collection", err)
	}
}

// fireScheduledChild creates the child Argo makes from parent at this instant.
func (f *composedFixture) fireScheduledChild(t *testing.T, parent *wf.CronWorkflow, suffix string) *wf.Workflow {
	t.Helper()
	ctx := argologging.WithLogger(t.Context(), scheduledFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-"+suffix, f.clock.Now())
	child.Namespace = f.config.PipelineNamespace
	child = initializedScheduledObservation(t, child, f.clock.Now())
	child, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Create(t.Context(), child, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func workflowParameter(spec wf.WorkflowSpec, name string) string {
	for _, p := range spec.Arguments.Parameters {
		if p.Name == name && p.Value != nil {
			return p.Value.String()
		}
	}
	return ""
}
