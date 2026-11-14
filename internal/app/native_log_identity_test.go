package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type nativeLogTestAuth struct{}

func (nativeLogTestAuth) Authenticate(context.Context, string) (api.Actor, error) {
	return api.Actor{Identity: "reader", Admin: true}, nil
}

// One Argo-shaped tree covers the naming boundaries that used to disagree:
// root-level leaf, nested DAG, both retry attempts, ContainerSet and onExit.
func nativeLogIdentityTree(failed string) (*wfv1.Workflow, []*corev1.Pod) {
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "native-workflow", Namespace: "pipeline", UID: "workflow-uid"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowFailed, Nodes: wfv1.Nodes{}}}
	add := func(id, name, boundary string, kind wfv1.NodeType, ordinal int) {
		at := metav1.NewTime(time.Unix(int64(100+ordinal), 0))
		wf.Status.Nodes[id] = wfv1.NodeStatus{ID: id, Name: wf.Name + "." + name, DisplayName: name, BoundaryID: boundary, Type: kind, Phase: wfv1.NodeSucceeded, StartedAt: at, FinishedAt: at, Outputs: &wfv1.Outputs{ExitCode: ptr.To("0")}}
	}
	add("root", wf.Name, "", wfv1.NodeTypeDAG, 0)
	root := wf.Status.Nodes["root"]
	root.Name = wf.Name
	wf.Status.Nodes["root"] = root
	add("flat-node", "flat", "root", wfv1.NodeTypePod, 1)
	add("nested", "nested", "root", wfv1.NodeTypeDAG, 2)
	add("nested-node", "shared", "nested", wfv1.NodeTypePod, 3)
	add("retry", "compile", "root", wfv1.NodeTypeRetry, 4)
	retry := wf.Status.Nodes["retry"]
	retry.Children = []string{"attempt-0", "attempt-1"}
	wf.Status.Nodes["retry"] = retry
	add("attempt-0", "compile(0)", "root", wfv1.NodeTypePod, 5)
	add("attempt-1", "compile(1)", "root", wfv1.NodeTypePod, 6)
	first := wf.Status.Nodes["attempt-0"]
	first.Phase = wfv1.NodeFailed
	first.Outputs.ExitCode = ptr.To("7")
	wf.Status.Nodes["attempt-0"] = first
	add("set-pod", "set", "root", wfv1.NodeTypePod, 7)
	add("set-build", "build", "set-pod", wfv1.NodeTypeContainer, 8)
	add("set-lint", "lint", "set-pod", wfv1.NodeTypeContainer, 9)
	add("exit-node", "cleanup", "", wfv1.NodeTypePod, 10)
	target := wf.Status.Nodes[failed]
	target.Phase = wfv1.NodeFailed
	target.Outputs.ExitCode = ptr.To("42")
	wf.Status.Nodes[failed] = target
	if failed == "set-build" {
		parent := wf.Status.Nodes["set-pod"]
		parent.Phase = wfv1.NodeFailed
		parent.Outputs.ExitCode = ptr.To("42")
		wf.Status.Nodes["set-pod"] = parent
	}
	var pods []*corev1.Pod
	for _, node := range wf.Status.Nodes {
		if node.Type != wfv1.NodeTypePod {
			continue
		}
		names := []corev1.Container{{Name: "main"}, {Name: "wait"}}
		if node.ID == "set-pod" {
			names = []corev1.Container{{Name: "build"}, {Name: "lint"}, {Name: "wait"}}
		}
		pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("opaque-%d", len(pods)), Namespace: wf.Namespace, UID: types.UID("pod-" + node.ID), Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: names}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}})
	}
	return wf, pods
}

func TestNativeNodeIdentityFlowsThroughRunAndLogs(t *testing.T) {
	for _, wanted := range []string{"flat-node", "nested-node", "attempt-1", "set-build", "exit-node"} {
		t.Run(wanted, func(t *testing.T) {
			ctx := t.Context()
			database, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "history.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "origin", Kind: "ssh", BaseURL: "ssh://git@origin.example/team"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := storetest.EnqueueRun(database, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "push"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = database.StartRun(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			wf, pods := nativeLogIdentityTree(wanted)
			if _, err = database.SetRunWorkflowName(ctx, run.ID, wf.Name); err != nil {
				t.Fatal(err)
			}
			completion := argojob.Completion{Name: wf.Name, Terminal: true, Phase: string(wf.Status.Phase), Steps: argojob.StepResults(wf), NodeSteps: argojob.NodeStepResults(wf, pods)}
			for _, step := range completion.Steps {
				if step.Status == runprogress.StepFailed {
					completion.FailedStage, completion.FailedStep = step.Stage, step.Step
					break
				}
			}
			result := (&ArgoJobs{}).result(completion)
			if result.FailedStep != wanted {
				t.Fatalf("failure identity split: got %q want native node %q", result.FailedStep, wanted)
			}
			if err = database.PutStepResults(ctx, run.ID, result.Steps); err != nil {
				t.Fatal(err)
			}
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var selected model.StepResult
			for _, row := range result.Steps {
				if row.NodeID == wanted {
					selected = row
				}
			}
			container := "main"
			if wanted == "set-build" {
				container = "build"
			}
			for _, binding := range argojob.StepContainers(wf, pods) {
				primary := binding.ContainerName != "wait"
				body := "[other/step] ordinary bytes\nError: " + binding.NodeID + " native output\n"
				entry := runlog.ContainerLog{NodeID: binding.NodeID, PodUID: string(binding.PodUID), PodName: binding.PodName, ContainerName: binding.ContainerName, Main: primary, Reader: strings.NewReader(body)}
				if err = logs.StoreContainerLog(run.ID, entry, 1<<20); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = database.FinishRun(ctx, run.ID, model.RunResult{Status: result.Status, Phase: result.Phase, FailedStage: result.FailedStage, FailedStep: result.FailedStep, Error: result.Error}); err != nil {
				t.Fatal(err)
			}
			control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Repositories: database, Logs: logs})
			if err != nil {
				t.Fatal(err)
			}
			defer control.Shutdown(context.Background())
			actor := api.Actor{Identity: "reader", Admin: true}
			expected := "[other/step] ordinary bytes\nError: " + wanted + " native output\n"
			for _, call := range []struct {
				name string
				args map[string]any
			}{{"run_logs", map[string]any{"id": run.ID}}, {"logs", map[string]any{"repo": repo.Name, "sha": run.SHA, "step": selected.Step}}, {"logs", map[string]any{"repo": repo.Name, "sha": run.SHA, "step": wanted}}, {"run_logs", map[string]any{"id": run.ID, "node_id": wanted, "container": container}}} {
				raw, _ := json.Marshal(call.args)
				value, err := control.CallTool(ctx, actor, call.name, raw)
				if err != nil {
					t.Fatal(call.name, call.args, err)
				}
				wire, _ := json.Marshal(value)
				var log service.LogResponse
				if err = json.Unmarshal(wire, &log); err != nil || log.Output != expected {
					t.Fatal(call.name, string(wire), err)
				}
			}
			// The CLI's default GET and the dashboard's exact-node GET share this route.
			server, err := api.New(nativeLogTestAuth{}, control, control, "test")
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"", "?step=" + url.QueryEscape(wanted)} {
				request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID+"/logs"+query, nil)
				request.Header.Set("Authorization", "Bearer fixture")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				var log service.LogResponse
				if err = json.Unmarshal(response.Body.Bytes(), &log); err != nil || response.Code != 200 || log.NodeID != wanted || log.Output != expected {
					t.Fatal(response.Code, response.Body.String(), err)
				}
			}
		})
	}
}

func TestNativeGreenRetryClearsFailureIdentity(t *testing.T) {
	wf, pods := nativeLogIdentityTree("flat-node")
	flat := wf.Status.Nodes["flat-node"]
	flat.Phase = wfv1.NodeSucceeded
	wf.Status.Nodes["flat-node"] = flat
	wf.Status.Phase = wfv1.WorkflowSucceeded
	completion := argojob.Completion{Name: wf.Name, Terminal: true, Succeeded: true, Phase: string(wf.Status.Phase), Steps: argojob.StepResults(wf), NodeSteps: argojob.NodeStepResults(wf, pods), FailedStage: "compile", FailedStep: "compile"}
	result := (&ArgoJobs{}).result(completion)
	if result.Status != model.RunPassed || result.FailedStep != "" || result.FailedStage != "" {
		t.Fatal("green retry retained failure identity", result)
	}
}
