package argoworkflow

import (
	"encoding/json"
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestAdmissionPeakDAGFixtures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial bool
		cap    int64
		want   int64
	}{
		{"serial chain", true, 0, 1000}, {"fan out", false, 0, 3000}, {"fan out capped", false, 2, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root"}}
			root := wfv1.Template{Name: "root", DAG: &wfv1.DAGTemplate{}}
			for i := 0; i < 3; i++ {
				name := fmt.Sprintf("task-%d", i)
				task := wfv1.DAGTask{Name: name, Template: name}
				if tc.serial && i > 0 {
					previous := fmt.Sprintf("task-%d", i-1)
					task.Depends = "(" + previous + ".Succeeded || " + previous + ".Failed || " + previous + ".Errored)"
				}
				root.DAG.Tasks = append(root.DAG.Tasks, task)
				wf.Spec.Templates = append(wf.Spec.Templates, declaredLeaf(name, "1", "1Gi"))
			}
			wf.Spec.Templates = append(wf.Spec.Templates, root)
			if tc.cap > 0 {
				wf.Spec.Parallelism = &tc.cap
			}
			d, err := DeclaredPodPeak(wf)
			if err != nil || d == nil {
				t.Fatalf("demand=%+v error=%v", d, err)
			}
			if d.CPUMilli != tc.want || d.MemoryBytes != tc.want/1000<<30 || d.Pods != tc.want/1000 {
				t.Errorf("peak=%+v, want %dm CPU and %d pods", d, tc.want, tc.want/1000)
			}
			raw, _ := json.Marshal(d)
			var view struct {
				DeclaredTotal *struct {
					CPUMilli    int64 `json:"cpu_milli"`
					MemoryBytes int64 `json:"memory_bytes"`
					Pods        int64 `json:"pods"`
				} `json:"declared_total"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			if view.DeclaredTotal == nil || view.DeclaredTotal.CPUMilli != 3000 || view.DeclaredTotal.MemoryBytes != 3<<30 || view.DeclaredTotal.Pods != 3 {
				t.Errorf("missing uncapped declaration total: %s", raw)
			}
		})
	}
}

func TestAdmissionPeakStaticLoopTotalIgnoresConcurrencyCap(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
		{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "workers", Template: "leaf", WithParam: "[1,2,3,4,5]"}}}}, declaredLeaf("leaf", "1", "1Gi"),
	}}}
	cap := int64(2)
	wf.Spec.Templates[0].Parallelism = &cap
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil {
		t.Fatalf("%+v %v", d, err)
	}
	raw, _ := json.Marshal(d)
	var view struct {
		DeclaredTotal struct {
			Pods int64 `json:"pods"`
		} `json:"declared_total"`
	}
	_ = json.Unmarshal(raw, &view)
	if d.Pods != 2 || view.DeclaredTotal.Pods != 5 {
		t.Fatalf("capped peak versus all iterations: %s", raw)
	}
}

func TestAdmissionPeakAmbiguousDependenciesKeepConcurrentDemand(t *testing.T) {
	for _, depends := range []string{"a.Succeeded || b.Succeeded", "a.Running || a.Succeeded", "!a.Succeeded", "a.Daemoned", "a.Succeeded && b.Succeeded || a.Failed", "a.", "((a.Succeeded)", "a.Succeeded &&"} {
		t.Run(depends, func(t *testing.T) {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
				{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf"}, {Name: "c", Template: "leaf", Depends: depends}}}}, declaredLeaf("leaf", "1", "1Gi"),
			}}}
			d, err := DeclaredPodPeak(wf)
			if err != nil || d == nil || d.CPUMilli != 3000 || d.Pods != 3 {
				t.Fatalf("ambiguous ordering undercounted: %+v %v", d, err)
			}
		})
	}
}

func TestAdmissionPeakParenthesizedAndSkippedDependencies(t *testing.T) {
	for _, depends := range []string{"(a.Succeeded || a.Skipped)", "((a.Succeeded || a.Skipped) && (a.Succeeded || a.Failed))"} {
		t.Run(depends, func(t *testing.T) {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
				{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf", Depends: depends}}}}, declaredLeaf("leaf", "1", "1Gi"),
			}}}
			d, err := DeclaredPodPeak(wf)
			if err != nil || d == nil || d.CPUMilli != 1000 || d.Pods != 1 {
				t.Fatalf("terminal dependency should serialize calls: %+v %v", d, err)
			}
		})
	}
}
