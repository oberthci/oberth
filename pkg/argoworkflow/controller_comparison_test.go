package argoworkflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestPreparedPlanCoversActualControllerLeaves(t *testing.T) {
	t.Parallel()
	directory := os.Getenv("OBERTH_CONTROLLER_OUTPUT")
	if directory == "" {
		var err error
		directory, err = os.MkdirTemp(os.Getenv("GOTMPDIR"), "oberth-controller-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(directory); err != nil {
				t.Error(err)
			}
		})
	}
	documents, err := filepath.Glob("testdata/controller/*.json")
	if err != nil {
		t.Fatal(err)
	}
	shared := &wfv1.WorkflowTemplate{}
	readControllerJSON(t, "testdata/controller/shared-object.json", shared)
	policy := Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": shared})}
	writeControllerJSON(t, directory, "phases", expectedControllerPhases())
	for _, document := range documents {
		name := strings.TrimSuffix(filepath.Base(document), ".json")
		if name == "shared-object" {
			writeControllerJSON(t, directory, name, shared)
			continue
		}
		wf := &wfv1.Workflow{}
		readControllerJSON(t, document, wf)
		request := requestFor(t, wf)
		casePolicy := policy
		if name == "script-containerset-resource" {
			_, fixture := resourceWorkflow("")
			casePolicy.Capability = fixture.Capability
			casePolicy.ServerNamespace, casePolicy.ArgoNamespace = fixture.ServerNamespace, fixture.ArgoNamespace
			request.IdentityRequest.ResourceAccount = "resources"
		}
		prepared, err := Prepare(t.Context(), request, casePolicy, identityHooks())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		writeControllerJSON(t, directory, name, wf)
		writeControllerJSON(t, directory, name+"-plan", prepared.View().Plan)
	}
	writeControllerSourceCopies(t, directory)
	runControllerComparison(t, directory)
	checkInstallerControllerStatus(t, directory)
}
func runControllerComparison(t *testing.T, directory string) {
	t.Helper()
	ctx := controllerContext(t)
	command := controllerFixture.Command(ctx, t, "-test.count=1", "-test.timeout=180s", "-test.run=^TestOberth(ControllerPlans|SourceCopiesArePodPrivate|InstallerStatus)$", "-test.v")
	command.Env = append(os.Environ(), "OBERTH_CONTROLLER_CASES="+directory, "TMPDIR="+os.Getenv("GOTMPDIR"))
	raw, err := controllerOutput(command)
	if err != nil {
		t.Fatalf("controller comparison: %v\n%s", err, raw)
	}
	observed, err := filepath.Glob(filepath.Join(directory, "*-observed.json"))
	if err != nil || len(observed) != len(expectedControllerPhases()) {
		t.Fatal("missing controller comparisons", len(observed), err)
	}
	triage, err := os.ReadFile(filepath.Join(directory, "inline-triage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inline resolver triage: %s", triage)
	for _, path := range observed {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(raw))
	}
}
func readControllerJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}
func writeControllerJSON(t *testing.T, directory, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func expectedControllerPhases() map[string][]string {
	return map[string][]string{
		"TestSecondDAGLoopExitGroup":            {"Succeeded"},
		"TestSecondDAGLoopLifecycle":            {"Succeeded"},
		"TestSecondDefaultExpansionBounded":     {"Running"},
		"TestSecondExitHookNativePath":          {"Succeeded"},
		"TestSecondExitPrecedence":              {"Succeeded"},
		"TestSecondRetriesAndLeafKinds":         {"Succeeded"},
		"TestSecondTemplateDefaultsKeepDAGType": {"Succeeded"},
		"continueon-loop-hooks":                 {"Succeeded"},
		"dag-ref-hooks-defaults":                {"Succeeded"},
		"dag-retry-hooks":                       {"Succeeded"},
		"depends-expression-loop-hooks":         {"Succeeded"},
		"nested-dag-steps-loops":                {"Succeeded"},
		"nested-steps-dag-loops":                {"Succeeded"},
		"onexit-with-steps-loop":                {"Succeeded"},
		"own-build-structure":                   {"Running"},
		"own-release-structure":                 {"Running"},
		"shared-consumer-defaults":              {"Succeeded"},
		"steps-ref-retry-hooks":                 {"Succeeded"},
		"target-loop-with-slow-sibling":         {"Running"},
		"withparam-previous-output":             {"Succeeded"},
		"workflow-lifecycle-exit":               {"Succeeded"},
		"inline-steps-in-dag-hooks":             {"Error", "Succeeded"},
		"sequence-dag-hooks":                    {"Succeeded"},
		"map-items-punctuation-hooks":           {"Succeeded"},
		"script-containerset-resource":          {"Running"},
	}
}
