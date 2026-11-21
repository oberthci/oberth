package argoworkflow

import (
	"context"
	"fmt"
	"strings"

	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	core "k8s.io/api/core/v1"
)

func TestNativeFaultCollectionIsBounded(t *testing.T) {
	for _, count := range []int{20, 21, 200, 600} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			w := simpleWorkflow()
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].DAG = &wf.DAGTemplate{}
			for i := range count {
				name := fmt.Sprintf("fault-%03d", i)
				w.Spec.Templates[0].DAG.Tasks = append(w.Spec.Templates[0].DAG.Tasks, wf.DAGTask{Name: name, Template: name})
				w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: name, Container: &core.Container{Image: "image", Args: []string{"{{inputs.parameters.missing}}"}}})
			}
			r := requestFor(t, w)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			_, err := Prepare(ctx, r, Policy{}, identityHooks())
			findings := admissionFindings(t, err)
			if count <= maxNativeFindings {
				if len(findings) != count {
					t.Fatal(findings)
				}
			} else if len(findings) != maxNativeFindings+1 || findings[0].Code != "native_more" || !strings.Contains(findings[0].Remedy, "sample") {
				t.Fatal("findings were not capped or truncation was hidden", findings)
			}
		})
	}
	t.Run("work-limit-keeps-findings", assertNativeWorkLimitRetainsFindings)
	t.Run("one-field-many-faults", assertNativePassesAreBounded)
}

func assertNativeWorkLimitRetainsFindings(t *testing.T) {
	t.Helper()
	w := simpleWorkflow()
	r := requestFor(t, w)
	g := newGraph(t.Context(), w, r, Policy{})
	g.build()
	c := nativeCollector{work: maxInspectionNodes, seen: map[admissioncontract.Finding]bool{}}
	c.add(finding("spec", "native"), "first fault")
	if err := c.scope(t.Context(), w, nil, g.nodes); err != nil || !c.stopped || len(c.result.Findings) != 2 || c.result.Findings[0].Code != "native" || c.result.Findings[1].Code != "native_work" || !strings.Contains(c.result.Detail, "first fault") {
		t.Fatal("work limit discarded a known fault", err, c.result)
	}
}

func assertNativePassesAreBounded(t *testing.T) {
	t.Helper()
	w := simpleWorkflow()
	var expressions []string
	for i := range maxNativeFindings + 1 {
		expressions = append(expressions, fmt.Sprintf("{{inputs.parameters.missing%d}}", i))
	}
	w.Spec.Templates[0].Container.Args = []string{strings.Join(expressions, " ")}
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	findings := admissionFindings(t, err)
	if len(findings) != 2 || findings[0].Code != "native_more" || findings[1].Code != "native" {
		t.Fatal("deduplication bypassed the native pass limit", err)
	}
}
