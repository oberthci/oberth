package controller

import (
	"encoding/json"
	"github.com/argoproj/argo-workflows/v4/config"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDelta2NativeHeldEvidenceShapes(t *testing.T) {
	t.Setenv("INFORMER_WRITE_BACK", "true")
	for _, name := range []string{"parent", "dynamic"} {
		t.Run(name, func(t *testing.T) {
			dir := os.Getenv("OBERTH_HOLD_CONTROLLER_CASES")
			raw, err := os.ReadFile(filepath.Join(dir, name+"-input.json"))
			if err != nil {
				t.Fatal(err)
			}
			var w wf.Workflow
			if err = json.Unmarshal(raw, &w); err != nil {
				t.Fatal(err)
			}
			w.Name = "comparison"
			w.GenerateName = ""
			w.Namespace = "default"
			sa := defaultServiceAccount.DeepCopy()
			sa.Name = w.Spec.Executor.ServiceAccountName
			ctx := logging.TestContext(t.Context())
			cancel, c := newController(ctx, &w, sa, defaultServiceAccount, func(c *WorkflowController) {
				c.eventRecorderManager = &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(10000)}
				c.Config.Images["example.test/build:1"] = config.Image{Cmd: []string{}}
				c.Config.Images["oberth.ci/helper:1"] = config.Image{Cmd: []string{}}
			})
			defer cancel()
			woc := newWorkflowOperationCtx(ctx, &w, c)
			wrapped := map[string]bool{}
			for range 20 {
				woc.operate(ctx)
				pods, err := c.kubeclientset.CoreV1().Pods(w.Namespace).List(ctx, meta.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pods.Items {
					for _, cn := range p.Spec.Containers {
						if cn.Name == "main" {
							wrapped[p.Annotations[common.AnnotationKeyNodeName]] = strings.Contains(strings.Join(append(cn.Command, cn.Args...), " "), "/oberth-hold/bin/oberth-hold")
						}
					}
				}
				makePodsPhase(ctx, woc, core.PodSucceeded, func(p *core.Pod, _ *wfOperationCtx) {
					node := p.Annotations[common.AnnotationKeyNodeName]
					fail := name == "parent" && node == "comparison.group(0).build" || name == "dynamic" && node == "comparison.loop(0:x).build"
					code := int32(0)
					if fail {
						p.Status.Phase = core.PodFailed
						code = 7
					}
					p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code}}}, {Name: "wait", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}}}
				})
				if woc.wf.Status.Phase.Completed() {
					break
				}
			}
			if woc.wf.Status.Phase != wf.WorkflowSucceeded {
				t.Fatalf("native did not pass: %s %s", woc.wf.Status.Phase, woc.wf.Status.Message)
			}
			failed, passed := 0, 0
			for _, n := range woc.wf.Status.Nodes {
				if n.Type == wf.NodeTypePod {
					t.Logf("native %s=%s wrapped=%v", n.Name, n.Phase, wrapped[n.Name])
					if wrapped[n.Name] {
						t.Fatalf("native Pod wrapped despite ineligibility: %s", n.Name)
					}
					if n.Phase == wf.NodeFailed {
						failed++
					}
					if n.Phase == wf.NodeSucceeded {
						passed++
					}
				}
			}
			wantPassed := 1
			if name == "dynamic" {
				wantPassed = 2
			}
			if failed != 1 || passed != wantPassed {
				t.Fatal("wrong native leaf outcomes", failed, passed)
			}
			raw, err = json.MarshalIndent(woc.wf, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, name+"-native.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
