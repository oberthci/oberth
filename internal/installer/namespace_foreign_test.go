package installer_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestForeignReleaseNamespace(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"server", "pipeline"} {
		for _, status := range []release.Status{release.StatusDeployed, release.StatusFailed, release.StatusPendingInstall} {
			for _, recheck := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/recheck=%t", role, status, recheck), func(t *testing.T) {
					foreignNamespaceCase(t, role, status, recheck)
				})
			}
		}
	}
}

func foreignNamespaceCase(t *testing.T, role string, status release.Status, recheck bool) {
	t.Helper()
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	target := "second"
	if role == "pipeline" {
		target = "jobs"
	}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: target, Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	record := foreignNamespaceRelease()
	record.Info.Status = status
	record.Manifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + target + "\n"
	saveForeignRelease(t, f, record)
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil || !strings.Contains(err.Error(), "control/fixture-owner") || !strings.Contains(err.Error(), target) {
		t.Fatal("foreign namespace holder was lost", err)
	}
	assertNoInstallWrites(t, f)
}

func foreignNamespaceRelease() *release.Release {
	return &release.Release{Name: "fixture-owner", Namespace: "control", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "fixture-chart", Version: "1.0.0"}}}
}

func saveForeignRelease(t *testing.T, f fixture, record *release.Release) {
	t.Helper()
	key := fmt.Sprintf("sh.helm.release.v1.%s.v%d", record.Name, record.Version)
	if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets(record.Namespace)).Create(key, record); err != nil {
		t.Fatal(err)
	}
}

func TestForeignNamespaceHooksListsAndUnreadableManifests(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"hook", "list", "manifest", "invalid-list", "invalid-name", "own-name"} {
		for _, recheck := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recheck=%t", shape, recheck), func(t *testing.T) { foreignManifestCase(t, shape, recheck) })
		}
	}
}

func foreignManifestCase(t *testing.T, shape string, recheck bool) {
	t.Helper()

	f := setup(t)
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	r := foreignNamespaceRelease()
	switch shape {
	case "own-name":
		r.Name, r.Namespace = "oberth", "oberth"
		r.Manifest = "kind: Namespace\nmetadata:\n  name: oberth-runs\n"
	case "hook":
		r.Hooks = []*release.Hook{{Manifest: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: oberth-runs\n"}}
	case "list":
		r.Manifest = "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Namespace\n  metadata:\n    name: oberth-runs\n"
	case "manifest":
		r.Manifest = "kind: Namespace\nmetadata: ["
	case "invalid-list":
		r.Manifest = "kind: List\nitems: [unreadable]"
	case "invalid-name":
		r.Manifest = "kind: Namespace\nmetadata:\n  name: [unreadable]"
	}
	saveForeignRelease(t, f, r)
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil || !strings.Contains(err.Error(), r.Namespace+"/"+r.Name) {
		t.Fatal("foreign manifest membership ignored", err)
	}
	assertNoInstallWrites(t, f)
}

func TestUnrelatedReleaseDoesNotSupplyProductAnswers(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := foreignNamespaceRelease()
	// A workload in a namespace does not give another chart ownership of that namespace.
	r.Name, r.Namespace = "oberth", "oberth"
	r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config\n  namespace: oberth-runs\n"
	r.Config = map[string]any{"pipeline": []any{"not product answers"}}
	saveForeignRelease(t, f, r)
	f.client.ClearActions()
	plan := prepare(t, f)
	plan.Close()
	assertNoInstallWrites(t, f)
}

func TestNamespaceMembershipKeepsLatestAndDeployedOnly(t *testing.T) {
	t.Parallel()
	for _, deployed := range []bool{false, true} {
		t.Run(fmt.Sprint(deployed), func(t *testing.T) {
			f := setup(t)
			r := foreignNamespaceRelease()
			r.Info.Status = release.StatusSuperseded
			if deployed {
				r.Info.Status = release.StatusDeployed
			}
			r.Manifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: oberth-runs\n"
			saveForeignRelease(t, f, r)
			next := foreignNamespaceRelease()
			next.Version, next.Info.Status = 2, release.StatusFailed
			saveForeignRelease(t, f, next)
			f.client.ClearActions()
			plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if (err != nil) != deployed {
				t.Fatal("wrong revision selection", err)
			}
			if plan != nil {
				plan.Close()
			}
			assertNoInstallWrites(t, f)
		})
	}
}
