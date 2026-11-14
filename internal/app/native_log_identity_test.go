package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofile "github.com/argoproj/argo-workflows/v4/util/file"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
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
	if strings.HasPrefix(failed, "duplicate-") {
		for _, side := range []string{"left", "right"} {
			add(side, side, "root", wfv1.NodeTypeDAG, 11)
			add(side+"-build", side+".build", side, wfv1.NodeTypeDAG, 12)
			boundary := wf.Status.Nodes[side+"-build"]
			boundary.DisplayName = "build"
			wf.Status.Nodes[boundary.ID] = boundary
			ordinal := 13
			if side == "right" {
				ordinal = 14
			}
			add("duplicate-"+side, side+".build.compile", side+"-build", wfv1.NodeTypePod, ordinal)
			leaf := wf.Status.Nodes["duplicate-"+side]
			leaf.DisplayName = "compile"
			wf.Status.Nodes[leaf.ID] = leaf
		}
	}
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
		pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("opaque-%d", len(pods)), Namespace: wf.Namespace, UID: types.UID("pod-" + node.ID), Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: names}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}})
	}
	return wf, pods
}

type nativeReaderCase struct {
	name, wanted     string
	compressed, wait bool
}

type nativeLogTestHub struct {
	workflow *wfv1.Workflow
	pods     []*corev1.Pod
}

func (hub nativeLogTestHub) Workflow(name string) (*wfv1.Workflow, bool) {
	return hub.workflow, name == hub.workflow.Name
}
func (hub nativeLogTestHub) PodsOf(string) []*corev1.Pod { return hub.pods }
func (hub nativeLogTestHub) Pods() []*corev1.Pod         { return hub.pods }
func (nativeLogTestHub) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}
func (nativeLogTestHub) Healthy() bool                     { return true }
func (nativeLogTestHub) Synced() bool                      { return true }
func (nativeLogTestHub) UnhealthySince() (time.Time, bool) { return time.Time{}, false }

func nativeReaderCompletion(t *testing.T, wf *wfv1.Workflow, pods []*corev1.Pod, test nativeReaderCase) argojob.Completion {
	t.Helper()
	snapshot := wf.DeepCopy()
	if test.compressed {
		body, err := json.Marshal(snapshot.Status.Nodes)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Status.CompressedNodes = argofile.CompressEncodeString(t.Context(), string(body))
		snapshot.Status.Nodes = nil
	}
	before := snapshot.DeepCopy()
	controller, err := argojob.NewController(&shutdownWorkflowFake{object: snapshot}, nil, argojob.Config{Namespace: wf.Namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"})
	if err != nil {
		t.Fatal(err)
	}
	var completion *argojob.Completion
	if test.wait {
		controller.WithWatchHub(nativeLogTestHub{workflow: snapshot, pods: pods})
		got, waitErr := controller.Wait(t.Context(), wf.Name, wf.Annotations["oberth.ci/run-id"], io.Discard)
		completion, err = &got, waitErr
	} else {
		completion, err = controller.TerminalState(t.Context(), wf.Name)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, before) {
		t.Fatal("native reader mutated cached Workflow")
	}
	return *completion
}

func TestNativeNodeIdentityFlowsThroughRunAndLogs(t *testing.T) {
	cases := []nativeReaderCase{}
	for _, wanted := range []string{"flat-node", "nested-node", "attempt-1", "set-build", "exit-node", "duplicate-left", "duplicate-right"} {
		cases = append(cases, nativeReaderCase{name: wanted, wanted: wanted})
	}
	for _, wanted := range []string{"flat-node", "duplicate-left", "duplicate-right", "set-build"} {
		cases = append(cases, nativeReaderCase{name: "compressed-terminal/" + wanted, wanted: wanted, compressed: true})
		cases = append(cases, nativeReaderCase{name: "compressed-wait/" + wanted, wanted: wanted, compressed: true, wait: true})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			wanted := test.wanted
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
			wf.Annotations = map[string]string{"oberth.ci/run-id": run.ID}
			completion := nativeReaderCompletion(t, wf, pods, test)
			result := (&ArgoJobs{}).result(completion)
			if result.FailedStep != wanted || completion.FailedNodeID != wanted {
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
			issue, err := database.OpenCIIssue(ctx, repo.ID, "main")
			if err != nil || !strings.Contains(issue.Body, "full step log: run_logs "+run.ID+" "+selected.Stage+" "+wanted) {
				t.Fatal("CI issue lost exact failure selector", issue, err)
			}
			control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Repositories: database, Logs: logs})
			if err != nil {
				t.Fatal(err)
			}
			defer control.Shutdown(context.Background())
			actor := api.Actor{Identity: "reader", Admin: true}
			detailValue, err := control.CallTool(ctx, actor, "run_get", json.RawMessage(`{"id":"`+run.ID+`","summary":false}`))
			if err != nil {
				t.Fatal(err)
			}
			detail := detailValue.(service.RunDetailResponse)
			if detail.Run.FailedStep != wanted {
				t.Fatal("status lost exact native failure or exit", detail)
			}
			statusArgs, _ := json.Marshal(map[string]string{"repo": repo.Name, "ref": run.SHA})
			statusValue, err := control.CallTool(ctx, actor, "status", statusArgs)
			if err != nil {
				t.Fatal(err)
			}
			status := statusValue.(service.StatusResponse)
			if status.FailedStep != wanted || status.ExitCode == nil || *status.ExitCode != 42 {
				t.Fatal("status lost native failure exit", status)
			}
			expected := "[other/step] ordinary bytes\nError: " + wanted + " native output\n"
			for _, call := range []struct {
				name string
				args map[string]any
			}{{"run_logs", map[string]any{"id": run.ID}}, {"logs", map[string]any{"repo": repo.Name, "sha": run.SHA, "step": selected.Step}}, {"logs", map[string]any{"repo": repo.Name, "sha": run.SHA, "step": wanted}}, {"run_logs", map[string]any{"id": run.ID, "node_id": wanted, "container": container}}} {
				raw, _ := json.Marshal(call.args)
				value, err := control.CallTool(ctx, actor, call.name, raw)
				if strings.HasPrefix(wanted, "duplicate-") && call.name == "logs" && call.args["step"] == selected.Step {
					if err == nil || !strings.Contains(err.Error(), "duplicate-left") || !strings.Contains(err.Error(), "duplicate-right") {
						t.Fatal("ambiguous display selector chose a branch", value, err)
					}
					continue
				}
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
	completion := argojob.Completion{Name: wf.Name, Terminal: true, Succeeded: true, Phase: string(wf.Status.Phase), Steps: argojob.StepResults(wf), NodeSteps: argojob.NodeStepResults(wf, pods), FailedStage: "compile", FailedStep: "compile", FailedNodeID: "attempt-0"}
	result := (&ArgoJobs{}).result(completion)
	if result.Status != model.RunPassed || result.FailedStep != "" || result.FailedStage != "" {
		t.Fatal("green retry retained failure identity", result)
	}
}

func TestNativeOffloadedResultRetainsVerdictAndReason(t *testing.T) {
	for _, phase := range []wfv1.WorkflowPhase{wfv1.WorkflowSucceeded, wfv1.WorkflowFailed} {
		for _, wait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wait=%t", phase, wait), func(t *testing.T) {
				wf, _ := nativeLogIdentityTree("flat-node")
				wf.Annotations = map[string]string{"oberth.ci/run-id": "run"}
				wf.Status.Phase, wf.Status.Nodes, wf.Status.OffloadNodeStatusVersion = phase, nil, "offloaded"
				completion := nativeReaderCompletion(t, wf, nil, nativeReaderCase{wait: wait})
				result := (&ArgoJobs{}).result(completion)
				want := model.RunFailed
				if phase == wfv1.WorkflowSucceeded {
					want = model.RunPassed
				}
				if result.Status != want || !strings.Contains(result.Error, "step rows are unavailable because Argo node status is offloaded") || result.TerminalObservation != nil || len(result.Steps) != 0 {
					t.Fatal(result)
				}
			})
		}
	}
}
