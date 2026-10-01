package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestNonBlockingTasksStatusFunctionDepends(t *testing.T) {
	// The release.yaml pattern: a downstream task handles the non-blocking
	// task's failure via (X.Succeeded || X.Failed || X.Errored).
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "publish", Template: "publish-tmpl", Depends: "build"},
				{Name: "homebrew", Template: "homebrew-tmpl", Depends: "publish"},
				{Name: "verify", Template: "verify-tmpl",
					Depends: "publish && (homebrew.Succeeded || homebrew.Failed || homebrew.Errored)"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "homebrew-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "verify-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if !nonBlocking["homebrew"] {
		t.Error("homebrew should be non-blocking: every reference handles its failure")
	}
	if nonBlocking["build"] {
		t.Error("build should be blocking: it has a bare reference in publish's depends")
	}
	if nonBlocking["publish"] {
		t.Error("publish should be blocking: it has a bare reference in verify's depends")
	}
	if nonBlocking["verify"] {
		t.Error("verify should be blocking: it is a leaf task with no dependents")
	}
}

func TestNonBlockingTasksBareReferenceIsBlocking(t *testing.T) {
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "setup", Template: "run"},
				{Name: "test", Template: "run", Depends: "setup"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking["setup"] {
		t.Error("setup should be blocking: it has a bare reference in test's depends")
	}
}

func TestNonBlockingTasksSucceededOnlyIsBlocking(t *testing.T) {
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "setup", Template: "run"},
				{Name: "test", Template: "run", Depends: "setup.Succeeded"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking["setup"] {
		t.Error("setup should be blocking: .Succeeded without .Failed requires success")
	}
}

func TestNonBlockingTasksFailedOnlyIsNonBlocking(t *testing.T) {
	// A task referenced only with .Failed is non-blocking: its failure
	// satisfies the expression rather than breaking it.
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "risky", Template: "run"},
				{Name: "cleanup", Template: "run", Depends: "risky.Failed"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if !nonBlocking["risky"] {
		t.Error("risky should be non-blocking: the only reference handles its failure")
	}
}

func TestNonBlockingTasksMixedReferences(t *testing.T) {
	// A task referenced with bare in one expression and status functions in
	// another is blocking: at least one downstream requires it to succeed.
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "run"},
				{Name: "publish", Template: "run", Depends: "build"},
				{Name: "cleanup", Template: "run", Depends: "build.Failed || build.Errored"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking["build"] {
		t.Error("build should be blocking: publish depends on it with a bare reference")
	}
}

func TestNonBlockingTasksOldDependenciesArray(t *testing.T) {
	// The old-style Dependencies array always implies .Succeeded.
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "setup", Template: "run"},
				{Name: "test", Template: "run", Dependencies: []string{"setup"}},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking["setup"] {
		t.Error("setup should be blocking: old-style Dependencies always requires success")
	}
}

func TestNonBlockingTasksNoEntrypointDAG(t *testing.T) {
	// A steps-based entrypoint has no DAG: all tasks are blocking by default.
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", Steps: []wfv1.ParallelSteps{
				{Steps: []wfv1.WorkflowStep{{Name: "step", Template: "run"}}},
			}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking != nil {
		t.Errorf("expected nil for non-DAG entrypoint, got %v", nonBlocking)
	}
}

func TestNonBlockingTasksLeafTaskIsBlocking(t *testing.T) {
	// A leaf task (no depends expression references it) is blocking.
	workflow := &wfv1.Workflow{}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "setup", Template: "run"},
				{Name: "finalize", Template: "run", Depends: "setup"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}

	nonBlocking := NonBlockingTasks(workflow)
	if nonBlocking["finalize"] {
		t.Error("finalize should be blocking: it is a leaf with no dependents")
	}
}

func TestNonBlockingTasksNilWorkflow(t *testing.T) {
	if NonBlockingTasks(nil) != nil {
		t.Error("expected nil for nil workflow")
	}
}

func TestParseDependsReferences(t *testing.T) {
	cases := []struct {
		name    string
		depends string
		want    map[string]dependsStatuses
	}{
		{
			name:    "bare reference",
			depends: "setup",
			want:    map[string]dependsStatuses{"setup": {bare: true}},
		},
		{
			name:    "status function",
			depends: "setup.Succeeded",
			want:    map[string]dependsStatuses{"setup": {succeeded: true}},
		},
		{
			name:    "multiple status functions",
			depends: "task.Succeeded || task.Failed || task.Errored",
			want:    map[string]dependsStatuses{"task": {succeeded: true, failed: true, errored: true}},
		},
		{
			name:    "mixed bare and status",
			depends: "a && (b.Succeeded || b.Failed)",
			want: map[string]dependsStatuses{
				"a": {bare: true},
				"b": {succeeded: true, failed: true},
			},
		},
		{
			name:    "complex expression",
			depends: "publish-chart && (publish-homebrew.Succeeded || publish-homebrew.Failed || publish-homebrew.Errored)",
			want: map[string]dependsStatuses{
				"publish-chart":    {bare: true},
				"publish-homebrew": {succeeded: true, failed: true, errored: true},
			},
		},
		{
			name:    "negation stripped",
			depends: "!task.Succeeded",
			want:    map[string]dependsStatuses{"task": {succeeded: true}},
		},
		{
			name:    "empty expression",
			depends: "",
			want:    map[string]dependsStatuses{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs := parseDependsReferences(tc.depends)
			for task, wantStatuses := range tc.want {
				got := refs[task]
				if got == nil {
					t.Errorf("task %q not found in refs", task)
					continue
				}
				if got.bare != wantStatuses.bare {
					t.Errorf("task %q bare = %v, want %v", task, got.bare, wantStatuses.bare)
				}
				if got.succeeded != wantStatuses.succeeded {
					t.Errorf("task %q succeeded = %v, want %v", task, got.succeeded, wantStatuses.succeeded)
				}
				if got.failed != wantStatuses.failed {
					t.Errorf("task %q failed = %v, want %v", task, got.failed, wantStatuses.failed)
				}
				if got.errored != wantStatuses.errored {
					t.Errorf("task %q errored = %v, want %v", task, got.errored, wantStatuses.errored)
				}
			}
			for task := range refs {
				if _, expected := tc.want[task]; !expected {
					t.Errorf("unexpected task %q in refs", task)
				}
			}
		})
	}
}
