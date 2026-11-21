package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/argoproj/argo-workflows/v4/config"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/argoproj/argo-workflows/v4/workflow/templateresolution"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

// This test runs alongside the pinned controller package so its own fake-client
// harness can reconcile the actual controller without exporting production seams.
func TestOberthControllerPlans(t *testing.T) {
	t.Setenv("INFORMER_WRITE_BACK", "true")
	directory := os.Getenv("OBERTH_CONTROLLER_CASES")
	documents, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents {
		name := strings.TrimSuffix(filepath.Base(document), ".json")
		if name == "shared-object" || name == "phases" || strings.HasSuffix(name, "-plan") || strings.HasPrefix(name, "source-copy-") {
			continue
		}
		t.Run(name, func(t *testing.T) { compareControllerPlan(t, directory, name) })
	}
}

func compareControllerPlan(t *testing.T, directory, name string) {
	var wf wfv1.Workflow
	readCase(t, filepath.Join(directory, name+".json"), &wf)
	wf.Name = "comparison"
	wf.GenerateName = ""
	wf.Namespace = "default"
	wf.Spec.Executor = &wfv1.ExecutorConfig{ServiceAccountName: "default"}
	addControllerSource(&wf, name)
	if patch := os.Getenv("OBERTH_CONTROLLER_POD_PATCH"); patch != "" {
		wf.Spec.PodSpecPatch = patch
	}
	var shared wfv1.WorkflowTemplate
	readCase(t, filepath.Join(directory, "shared-object.json"), &shared)
	shared.Namespace = "default"
	ctx := logging.TestContext(t.Context())
	cancel, controller := newController(ctx, &wf, defaultServiceAccount, &shared, func(c *WorkflowController) {
		configureControllerSource(c)
		c.eventRecorderManager = &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(10000)}
		for _, image := range []string{"image", "example.test/default:1", "example.test/default:2"} {
			c.Config.Images[image] = config.Image{Entrypoint: []string{"true"}}
		}
	})
	defer cancel()
	woc := newWorkflowOperationCtx(ctx, &wf, controller)
	var mounts []string
	var sourceSeen bool
	for range 12 {
		woc.operate(ctx)
		current, source := controllerMountPaths(t, woc)
		mounts = append(mounts, current...)
		sourceSeen = sourceSeen || source
		makePodsPhase(ctx, woc, corev1.PodSucceeded, func(pod *corev1.Pod, _ *wfOperationCtx) {
			node := pod.Annotations[common.AnnotationKeyNodeName]
			if ((name == "continueon-loop-hooks" || name == "depends-expression-loop-hooks") && node == "comparison.run(0:a)") || (name == "dag-retry-hooks" && node == "comparison.run(0)") {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Message = "simulated item or retry failure"
			}
		})
		for id, node := range woc.wf.Status.Nodes {
			if node.TemplateName == "producer" {
				node.Outputs = &wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "items", Value: wfv1.AnyStringPtr(`["a","b"]`)}}}
				woc.wf.Status.Nodes[id] = node
			}
		}
		if woc.wf.Status.Phase.Completed() {
			break
		}
	}
	if name == "own-build-structure" && !sourceSeen {
		t.Fatal("controller created no source driver Pod")
	}
	if name == "inline-steps-in-dag-hooks" {
		checkInlineResolverCandidates(t, woc.wf, directory)
	}
	compareObservedLeaves(t, directory, name, woc.wf)
	raw, err := json.Marshal(woc.wf)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+"-status.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	writeControllerMounts(t, directory, name, mounts)
}

// Step containers share their mount namespace with the authored process. Native
// helpers' private scratch mounts do not; their executor token does need protection.
// Input artifact mounts use their declared paths, outside the private mount inventory.
func controllerMountPaths(t *testing.T, woc *wfOperationCtx) ([]string, bool) {
	t.Helper()
	pods, err := woc.controller.kubeclientset.CoreV1().Pods(woc.wf.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var mounts []string
	var sourceSeen bool
	for _, pod := range pods.Items {
		sourceSeen = checkControllerSourcePod(t, &pod) || sourceSeen
		for _, c := range append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...) {
			mounts = append(mounts, controllerContainerMounts(c, woc.controller.executorImage())...)
		}
	}
	return mounts, sourceSeen
}

func controllerContainerMounts(c corev1.Container, executorImage string) []string {
	var paths []string
	step := c.Image != executorImage && !common.IsArgoSidecar(c.Name) && !common.IsArtifactPluginInit(c.Name)
	for _, mount := range c.VolumeMounts {
		if step && mount.Name != "input-artifacts" || mount.Name == common.ServiceAccountTokenVolumeName {
			paths = append(paths, mount.MountPath)
		}
	}
	return paths
}

func writeControllerMounts(t *testing.T, directory, name string, mounts []string) {
	t.Helper()
	slices.Sort(mounts)
	raw, err := json.Marshal(slices.Compact(mounts))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+"-mounts.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func readCase(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}
func compareObservedLeaves(t *testing.T, directory, name string, wf *wfv1.Workflow) {
	t.Helper()
	var phases map[string][]wfv1.WorkflowPhase
	readCase(t, filepath.Join(directory, "phases.json"), &phases)
	if !slices.Contains(phases[name], wf.Status.Phase) {
		t.Errorf("native phase %s; expected %v: %s", wf.Status.Phase, phases[name], wf.Status.Message)
	}
	if name == "inline-steps-in-dag-hooks" && wf.Status.Phase == wfv1.WorkflowError {
		outer, err := wf.GetNodeByName("comparison.outer")
		if err != nil || !strings.Contains(outer.Message, "failed to get a template") {
			t.Fatalf("unexpected inline resolver error: %v", err)
		}
	}
	var plan struct {
		Entries []struct {
			Path    string `json:"plan_path"`
			Dynamic bool
		}
	}
	readCase(t, filepath.Join(directory, name+"-plan.json"), &plan)
	retry := regexp.MustCompile(`\([0-9]+\)`)
	leaves := map[string]bool{}
	for _, node := range wf.Status.Nodes {
		if node.Type == wfv1.NodeTypePod || node.Type == wfv1.NodeTypeSuspend {
			path := strings.TrimPrefix(strings.TrimPrefix(node.Name, "comparison"), ".")
			path = retry.ReplaceAllString(path, "")
			leaves[path] = true
			covered := false
			for _, entry := range plan.Entries {
				covered = covered || entry.Path == path || entry.Dynamic && (entry.Path == "" || strings.HasPrefix(path, entry.Path+"(") || strings.HasPrefix(path, entry.Path+".") || strings.HasPrefix(path, entry.Path+"["))
			}
			if !covered {
				t.Errorf("controller leaf %q absent from admitted plan", path)
			}
		}
	}
	if len(leaves) == 0 {
		t.Fatal("controller ran no leaf", wf.Status.Phase, wf.Status.Message)
	}
	paths := []string{}
	for path := range leaves {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := struct {
		Name    string
		Phase   wfv1.WorkflowPhase
		Ran     []string
		Planned int
		Message string
	}{name, wf.Status.Phase, paths, len(plan.Entries), wf.Status.Message}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+"-observed.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(raw))
}

// The native aggregation scans this map and resolves its first matching child.
// Inline children lack a template name; their named hooks also match the prefix.
func checkInlineResolverCandidates(t *testing.T, wf *wfv1.Workflow, directory string) {
	t.Helper()
	ctx := logging.TestContext(t.Context())
	resolver := templateresolution.NewContext(nil, nil, wf, wf, logging.RequireLoggerFromContext(ctx))
	outer, err := wf.GetNodeByName("comparison.outer")
	if err != nil {
		t.Fatal(err)
	}
	var named, unnamed []string
	for _, node := range wf.Status.Nodes {
		if node.BoundaryID != outer.ID || !strings.HasPrefix(node.Name, "comparison.outer[0].inner(") || node.Type == wfv1.NodeTypeSkipped {
			continue
		}
		_, _, _, err := resolver.ResolveTemplate(ctx, &node)
		if node.TemplateName == "" {
			if err == nil || !strings.Contains(err.Error(), "failed to get a template") {
				t.Fatal("unexpected inline-holder resolution", node.Name, err)
			}
			unnamed = append(unnamed, node.Name)
		} else {
			if err != nil {
				t.Fatal("named hook failed resolution", node.Name, err)
			}
			named = append(named, node.Name)
		}
	}
	if len(named) == 0 || len(unnamed) == 0 {
		t.Fatal("native map did not contain both holder forms", named, unnamed)
	}
	sort.Strings(named)
	sort.Strings(unnamed)
	raw, err := json.Marshal(map[string]any{"named": named, "unnamed": unnamed, "outer_message": outer.Message})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "inline-triage.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("native map candidates: named=%q unnamed=%q", named, unnamed)
}
