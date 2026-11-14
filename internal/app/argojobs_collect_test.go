package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

// Exercise node projection, the real Argo adapter and scheduler finalization,
// including PutStepResult in a file-backed store. Controller completion and
// progress emission are supplied by the fixture; source planning and admission
// are outside this test. Unfinished nodes are progress, never durable outcomes.
func TestArgoCollectPreservesNativeVerdictAndUnfinishedNodes(t *testing.T) {
	for _, phase := range []wfv1.NodePhase{wfv1.NodePending, wfv1.NodeRunning} {
		for _, succeeded := range []bool{false, true} {
			t.Run(string(phase)+"/succeeded="+map[bool]string{true: "true", false: "false"}[succeeded], func(t *testing.T) {
				ctx := t.Context()
				db, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"), store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "github", Kind: "github", BaseURL: "https://github.com/org"})
				if err != nil {
					t.Fatal(err)
				}
				repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				logs, err := runlog.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				at := metav1.NewTime(time.Unix(1000, 0).UTC())
				code := "42"
				wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf"}, Status: wfv1.WorkflowStatus{Nodes: wfv1.Nodes{
					"root":       {ID: "root", Name: "wf", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
					"passed":     {ID: "passed", Name: "wf.cohort", DisplayName: "cohort", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded, StartedAt: at, FinishedAt: at},
					"unfinished": {ID: "unfinished", Name: "wf.tools-dir", DisplayName: "tools-dir", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: phase, StartedAt: at},
					"failed":     {ID: "failed", Name: "wf.verify", DisplayName: "verify", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, StartedAt: at, FinishedAt: at, Outputs: &wfv1.Outputs{ExitCode: &code}},
				}}}
				if phase == wfv1.NodePending {
					n := wf.Status.Nodes["unfinished"]
					n.StartedAt = metav1.Time{}
					wf.Status.Nodes["unfinished"] = n
				}
				wf.Status.Phase = wfv1.WorkflowFailed
				if succeeded {
					wf.Status.Phase = wfv1.WorkflowSucceeded
					delete(wf.Status.Nodes, "failed")
				}
				completion := argojob.Completion{Name: wf.Name, Terminal: true, Succeeded: succeeded, Phase: string(wf.Status.Phase), Reason: "original workflow failure", Steps: argojob.StepResults(wf), NodeSteps: argojob.NodeStepResults(wf, nil)}
				if !succeeded {
					completion.FailedStage, completion.FailedStep = "verify", "verify"
				}
				control := &configuredController{waitResult: completion}
				adapter := &ArgoJobs{controller: control, intents: map[string]argoIntent{}}
				jobs := &collectJobs{adapter: adapter, logs: logs, completion: completion}
				scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: db, Git: collectGit{}, Logs: logs, Jobs: jobs, Auditor: db, WorkspaceRoot: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				run, err := scheduler.EnqueueCI(ctx, service.CIRequest{EventID: "collect", Repository: repo, Branch: "feature/collect", SHA: strings.Repeat("a", 40), Actor: "test"})
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					current, err := db.Run(ctx, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Status.Terminal() {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("observer did not finish")
					}
					time.Sleep(time.Millisecond)
				}
				finished, err := db.Run(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := model.RunFailed
				if succeeded {
					wantStatus = model.RunPassed
				}
				if finished.Status != wantStatus || finished.Phase == "collect" {
					t.Fatalf("Workflow verdict replaced by node projection: %+v", finished)
				}
				if !succeeded && (finished.FailedStage != "wf" || finished.FailedStep != "failed" || finished.Error != "original workflow failure") {
					t.Fatalf("failure evidence changed: %+v", finished)
				}
				if succeeded && (finished.Error != "" || finished.FailedStep != "") {
					t.Fatal("green Workflow acquired an unfinished-node failure", finished)
				}
				rows, err := db.StepResults(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != len(wf.Status.Nodes) {
					t.Fatalf("results=%+v, want every native node", rows)
				}
				for _, row := range rows {
					if row.NodeID == "unfinished" && (row.Phase != string(phase) || row.FinishedAt != nil) {
						t.Fatalf("unfinished native state changed: %+v", row)
					}
					if row.NodeID == "failed" && (row.ExitCode != 42 || row.Status != model.StepFailed) {
						t.Fatalf("lost failed exit: %+v", row)
					}
				}

			})
		}
	}
}

type collectGit struct{ service.WorkspaceGit }

func (collectGit) RemoteRef(context.Context, string, string) (string, bool, error) {
	return strings.Repeat("a", 40), true, nil
}

func (collectGit) Checkout(_ context.Context, _, _, path string) error {
	return os.MkdirAll(path, 0700)
}
func (collectGit) ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error) {
	return nil, false, nil
}
func (collectGit) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "", nil, false, nil
}

type collectJobs struct {
	adapter    *ArgoJobs
	logs       *runlog.Store
	completion argojob.Completion
}

func (j *collectJobs) CreateWorkflow(_ context.Context, r service.WorkflowRequest) error {
	j.adapter.intents[r.WorkflowName] = argoIntent{runID: r.Run.ID, created: true}

	return nil
}

// Hide source planning/admission interfaces: this fixture tests finalization.
func (j *collectJobs) Wait(ctx context.Context, name string, w io.Writer) (service.JobResult, error) {
	return j.adapter.Wait(ctx, name, w)
}

func (j *collectJobs) Stop(ctx context.Context, name, runID string) error {
	return j.adapter.Stop(ctx, name, runID)
}
func (j *collectJobs) TerminalResult(ctx context.Context, name string) (service.JobResult, error) {
	return j.adapter.TerminalResult(ctx, name)
}

func (collectGit) MirrorBranchMatches(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (j *collectJobs) Terminate(ctx context.Context, name, id string) error {
	return j.adapter.Terminate(ctx, name, id)
}
