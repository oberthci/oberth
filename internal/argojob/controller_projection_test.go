package argojob_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestProjectionAgainstActualControllerStatuses(t *testing.T) {
	t.Parallel()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("controller harness requires an outer test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(-5*time.Second))
	defer cancel()
	directory := t.TempDir()
	prepared, _, _ := fixture(t)
	command := exec.CommandContext(ctx, "go", "test", "-p=1", "-buildvcs=false", "-count=1", "-timeout=240s", "-run", "^TestPreparedPlanCoversActualControllerLeaves$", "./pkg/argoworkflow")
	command.Dir = "../.."
	command.Env = append(os.Environ(), "OBERTH_CONTROLLER_OUTPUT="+directory, "OBERTH_CONTROLLER_POD_PATCH="+prepared.View().Workflow.Spec.PodSpecPatch, "OBERTH_CONTROLLER_DEADLINE="+deadline.Add(-10*time.Second).Format(time.RFC3339Nano))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	defer func() {
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	}()
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("controller harness: %v\n%s", err, raw)
	}
	checkControllerMounts(t, directory)
	names := []string{"TestSecondDAGLoopExitGroup", "TestSecondDAGLoopLifecycle", "TestSecondDefaultExpansionBounded", "TestSecondExitHookNativePath", "TestSecondTemplateDefaultsKeepDAGType", "continueon-loop-hooks", "withparam-previous-output", "dag-retry-hooks"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { checkControllerProjection(t, directory, name) })
	}
}
func checkControllerProjection(t *testing.T, directory, name string) {
	t.Helper()
	var w wf.Workflow
	var plan admissioncontract.Plan
	readNativeJSON(t, filepath.Join(directory, name+"-status.json"), &w)
	readNativeJSON(t, filepath.Join(directory, name+"-plan.json"), &plan)
	out := project(t, argojob.Projection{Workflow: &w, Plan: plan})
	expected := controllerStepPhases(&w)
	actual := map[string]stepmodel.StepStatus{}
	for _, s := range out.Steps {
		if _, duplicate := actual[s.Path]; duplicate {
			t.Fatal("duplicate step", s.Path)
		}
		actual[s.Path] = s.Status
		if phase, ok := expected[s.Path]; ok {
			if s.Status != phase {
				t.Errorf("%s: got %s want %s", s.Path, s.Status, phase)
			}
		} else if s.AttemptCount != 0 {
			t.Error("unexpected native step", s)
		}
	}
	comparison := controllerComparison{actual: actual, expected: expected, terminal: w.Status.Phase.Completed()}
	for _, entry := range plan.Entries {
		checkPlannedEntry(t, entry, comparison)
	}
}

type controllerComparison struct {
	actual, expected map[string]stepmodel.StepStatus
	terminal         bool
}

func checkPlannedEntry(t *testing.T, entry admissioncontract.PlanEntry, c controllerComparison) {
	t.Helper()
	if entry.Dynamic {
		checkDynamicEntry(t, entry.Path, c)
		return
	}
	want, ok := c.expected[entry.Path]
	if !ok {
		want = stepmodel.StepPending
		if c.terminal {
			want = stepmodel.StepSkipped
		}
	}
	if c.actual[entry.Path] != want {
		t.Errorf("planned %q: got %s want %s", entry.Path, c.actual[entry.Path], want)
	}
}
func checkDynamicEntry(t *testing.T, base string, c controllerComparison) {
	t.Helper()
	expanded := false
	for path := range c.expected {
		if strings.HasPrefix(path, base) {
			expanded = true
			if _, ok := c.actual[path]; !ok {
				t.Error("missing expansion", path)
			}
		}
	}
	if expanded {
		if _, ok := c.actual[base]; ok {
			t.Error("expanded placeholder retained", base)
		}
		return
	}
	want := stepmodel.StepPending
	if c.terminal {
		want = stepmodel.StepSkipped
	}
	if c.actual[base] != want {
		t.Error("missing dynamic placeholder", base, c.actual[base], want)
	}
}
func readNativeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	compositionOK(t, err)
	compositionOK(t, json.Unmarshal(raw, v))
}
func controllerStepPhases(w *wf.Workflow) map[string]stepmodel.StepStatus {
	excluded := earlierControllerAttempts(w)
	retry := regexp.MustCompile(`\([0-9]+\)`)
	out := map[string]stepmodel.StepStatus{}
	for _, n := range w.Status.Nodes {
		if n.Type != wf.NodeTypePod && n.Type != wf.NodeTypeSuspend && n.Type != wf.NodeTypeSkipped {
			continue
		}
		if excludedNativeName(n.Name, excluded) {
			continue
		}
		path := retry.ReplaceAllString(strings.TrimPrefix(strings.TrimPrefix(n.Name, w.Name), "."), "")
		phases := map[wf.NodePhase]stepmodel.StepStatus{wf.NodePending: stepmodel.StepQueued, wf.NodeRunning: stepmodel.StepRunning, wf.NodeSucceeded: stepmodel.StepPassed, wf.NodeFailed: stepmodel.StepFailed, wf.NodeError: stepmodel.StepFailed, wf.NodeSkipped: stepmodel.StepSkipped, wf.NodeOmitted: stepmodel.StepSkipped}
		out[path] = phases[n.Phase]
	}
	return out
}
func earlierControllerAttempts(w *wf.Workflow) []string {
	excluded := []string{}
	for _, n := range w.Status.Nodes {
		if n.Type != wf.NodeTypeRetry {
			continue
		}
		attempts := []string{}
		for _, id := range n.Children {
			if child := w.Status.Nodes[id]; strings.HasPrefix(child.Name, n.Name+"(") {
				attempts = append(attempts, child.Name)
			}
		}
		if len(attempts) > 1 {
			excluded = append(excluded, attempts[:len(attempts)-1]...)
		}
	}
	return excluded
}
func excludedNativeName(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
