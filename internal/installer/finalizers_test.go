package installer_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRemovalReleasesOnlyThisInstallationsArchiveFinalizers(t *testing.T) {
	t.Parallel()
	objects := []*wf.Workflow{}
	for _, name := range []string{"owned", "foreign", "other-engine"} {
		w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "jobs", UID: types.UID(name), ResourceVersion: "1", Labels: map[string]string{"oberth.ci/engine": "argo", "workflows.argoproj.io/controller-instanceid": "frontdoor"}, Finalizers: []string{"oberth.ci/run-record", "other.ci/guard"}}}
		if name == "foreign" {
			w.Labels["workflows.argoproj.io/controller-instanceid"] = "other"
		}
		if name == "other-engine" {
			w.Labels["oberth.ci/engine"] = "other"
		}
		objects = append(objects, w)
	}
	client := argofake.NewSimpleClientset(objects[0], objects[1], objects[2])
	parent := &wf.CronWorkflow{ObjectMeta: meta.ObjectMeta{Name: "owned-parent", Namespace: "jobs", UID: "parent-uid", ResourceVersion: "1", Labels: map[string]string{"app.kubernetes.io/managed-by": "oberth", "workflows.argoproj.io/controller-instanceid": "frontdoor"}}, Spec: wf.CronWorkflowSpec{WorkflowMetadata: &meta.ObjectMeta{Finalizers: []string{"oberth.ci/run-record", "other.ci/guard"}}}}
	if _, err := client.ArgoprojV1alpha1().CronWorkflows("jobs").Create(t.Context(), parent, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := installer.ReleaseRunFinalizers(t.Context(), client, installer.FinalizerScope{Namespace: "jobs", Instance: "frontdoor"}); err != nil {
		t.Fatal(err)
	}
	for _, before := range objects {
		after, err := client.ArgoprojV1alpha1().Workflows("jobs").Get(t.Context(), before.Name, meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := before.Finalizers
		if before.Name == "owned" {
			want = []string{"other.ci/guard"}
		}
		if !slices.Equal(want, after.Finalizers) {
			t.Fatal("removal released the wrong finalizers", before.Name, after.Finalizers)
		}
	}
	after, err := client.ArgoprojV1alpha1().CronWorkflows("jobs").Get(t.Context(), parent.Name, meta.GetOptions{})
	if err != nil || !slices.Equal(after.Spec.WorkflowMetadata.Finalizers, []string{"other.ci/guard"}) {
		t.Fatal("future schedule children still inherit archive guards", after, err)
	}
}

func TestRollbackReleasesArchiveFinalizersBeforeEarlierChart(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	before := len(f.charts.Applies)
	cleared := false
	passes := 0
	f.engine.Finalizers = func(_ context.Context, scope installer.FinalizerScope) error {
		if (passes == 0 && len(f.charts.Applies) != before) || scope.Namespace != "oberth-runs" || scope.Instance != "oberth" {
			t.Error("rollback changed chart or finalizer scope first", scope)
		}
		cleared = true
		passes++
		return nil
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !cleared || passes != 2 || len(f.charts.Applies) <= before {
		t.Fatal("rollback skipped finalizer release before chart change", cleared)
	}
}

func TestUninstallRunsScopedFinalizerHookBeforeDeletingResources(t *testing.T) {
	t.Parallel()
	job := object(t, renderedIn(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}}, "frontdoor"), "Job", "oberth-release-finalizers")
	if job["spec"].(map[string]any)["activeDeadlineSeconds"] != float64(660) {
		t.Fatal("uninstall deadline cannot drain a realistic backlog")
	}
	annotations := job["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["helm.sh/hook"] != "pre-delete" {
		t.Fatal("uninstall can delete namespace before releasing archive guards", annotations)
	}
	spec := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	if spec["serviceAccountName"] != "oberth" || !reflect.DeepEqual(c["args"], []any{"internal-finalizers", "jobs", "frontdoor"}) {
		t.Fatal("uninstall finalizer hook lost installation scope", spec)
	}
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	text := strings.Join(p.Plan.Review(), "\n")
	if !strings.Contains(text, "64 MiB per finished run") || !strings.Contains(text, "release archive finalizers first") {
		t.Fatal("upgrade review omits backfill or removal consequences", text)
	}
}
