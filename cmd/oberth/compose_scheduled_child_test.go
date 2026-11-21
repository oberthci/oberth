//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//nolint:gochecknoglobals // Construct before tests: the native logger mutates process-wide initialization state.
var scheduledFixtureLogger = argologging.NewSlogLogger(argologging.Warn, argologging.Text)

func TestCompositionRetainsScheduledAdmission(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledObservation(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.scheduledParent(t, commit)
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatal("parent preparation accepted a run", runs, err)
	}
	adapter := scheduleChildren{f.owners.c}
	if _, found, _, err := adapter.ChildBinding(t.Context(), nil); err != nil || found {
		t.Fatal("non-child observation was adopted", err)
	}
	if err = adapter.VerifyChild(nil, nativebinding.ChildBinding{}); err == nil {
		t.Fatal("schedule owner did not verify child custody")
	}
}

func TestComposedWatchReportsRefusedScheduledChild(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledObservation(t)
	observed := make(chan struct{})
	build := f.deps.constructors.NativeNew
	f.deps.constructors.NativeNew = func(ctx context.Context, c *composition) (Part, error) {
		c.diagnostic = &childRefusalLog{Writer: c.diagnostic, observed: observed}
		return build(ctx, c)
	}
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	ctx := argologging.WithLogger(t.Context(), scheduledFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-456", f.clock.Now())
	child.Namespace = f.config.PipelineNamespace
	child.OwnerReferences[0].UID = "foreign"
	_, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Create(t.Context(), child, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait, cancelWait := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWait()
	select {
	case <-observed:
	case <-wait.Done():
		t.Fatal("native watch did not report refused child", wait.Err())
	}
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatal("refused child accepted a run", runs, err)
	}
}

type childRefusalLog struct {
	io.Writer
	observed chan struct{}
	once     sync.Once
}

func (w *childRefusalLog) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if strings.Contains(string(p), "scheduled child not recorded") && strings.Contains(string(p), "-456") {
		w.once.Do(func() { close(w.observed) })
	}
	return n, err
}

func (f *composedFixture) prepareScheduledObservation(t *testing.T) string {
	t.Helper()
	return f.prepareScheduledDocument(t, strings.ReplaceAll(starterWorkflow(), containerimage.Utility, "oberth.ci/build:1"))
}

// prepareScheduledDocument commits document as the build pipeline with one
// nightly schedule on the default branch and returns that commit.
func (f *composedFixture) prepareScheduledDocument(t *testing.T, document string) string {
	t.Helper()
	f.prepareCommit(t)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	f.git.Write(t, schedule.FileName, []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]\n"))
	commit := f.git.CommitFile(t, "scheduled admission")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	name, err := identityname.Name(identityname.Key{Repository: gittest.Repository}, identityname.PipelineCI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.kube.CoreV1().ServiceAccounts(f.config.PipelineNamespace).Create(t.Context(), &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name}}, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func (f *composedFixture) scheduledParent(t *testing.T, commit string) *wf.CronWorkflow {
	t.Helper()
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentName, err := nativebinding.CronName(repo.ID, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := f.argo.ArgoprojV1alpha1().CronWorkflows(f.config.PipelineNamespace).Get(t.Context(), parentName, meta.GetOptions{})
	if err != nil {
		t.Fatal("schedule was not admitted", err, f.trace.String())
	}
	digest := parent.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation]
	retained, err := f.owners.c.store.ScheduleAdmission(t.Context(), repo.ID, "nightly", digest)
	if err != nil || retained.CommitSHA != commit {
		t.Fatal("native parent has no retained admission", err)
	}
	return parent
}
