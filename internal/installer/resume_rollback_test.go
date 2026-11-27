package installer_test

import (
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResumeDowngradeReviewNamesCancellationAndOrphanCleanup(t *testing.T) {
	t.Parallel()
	_, m := upgradeWithServicePort(t, releaseFor(t, "v0.30.99", 6))
	lines, err := m.RollbackReview()
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{
		Name: "orphan", Namespace: "jobs", UID: "orphan-uid", ResourceVersion: "1",
		Labels:     map[string]string{"oberth.ci/engine": "argo", "workflows.argoproj.io/controller-instanceid": "frontdoor"},
		Finalizers: []string{"oberth.ci/run-record", "other.ci/guard"},
	}}
	for _, required := range []string{"cancel non-terminal resumed runs or wait for them to finish", w.Finalizers[0] + " finalizer removed by hand"} {
		if !strings.Contains(text, required) {
			t.Fatal("rollback review lost resume prerequisite", text)
		}
	}
	client := argofake.NewSimpleClientset(w)
	if err = installer.ReleaseRunFinalizers(t.Context(), client, installer.FinalizerScope{Namespace: w.Namespace, Instance: "frontdoor"}); err != nil {
		t.Fatal(err)
	}
	after, err := client.ArgoprojV1alpha1().Workflows(w.Namespace).Get(t.Context(), w.Name, meta.GetOptions{})
	if err != nil || len(after.Finalizers) != 1 || after.Finalizers[0] != "other.ci/guard" {
		t.Fatal("review remedy differs from native Workflow finalizer release", after, err)
	}
}
