package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMissingNamespaceMarksUseReleaseMembership(t *testing.T) {
	t.Parallel()
	for _, marks := range []string{"absent", "partial", "other manager"} {
		for _, role := range []string{"server", "pipeline"} {
			for _, recheck := range []bool{false, true} {
				t.Run(marks+"/"+role+map[bool]string{true: " recheck"}[recheck], func(t *testing.T) { missingNamespaceMarksCase(t, []string{marks, role}, recheck) })
			}
		}
	}
}
func missingNamespaceMarksCase(t *testing.T, shape []string, recheck bool) {
	t.Helper()
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", HTTPSNodePort: 31443, SSHNodePort: 31022}
	if shape[1] == "server" {
		f.choices.Namespace = "a-runs"
	} else {
		f.choices.PipelineNamespace = "a-runs"
	}
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a-runs"}}
	if shape[0] == "partial" {
		ns.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth"}
	}
	if shape[0] == "other manager" {
		ns.Labels = map[string]string{"app.kubernetes.io/managed-by": "fixture-manager"}
	}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	addMembershipRelease(t, f, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a-runs\n")
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil || !strings.Contains(err.Error(), "a/oberth") || !strings.Contains(err.Error(), "a-runs") {
		t.Fatal("release membership was lost", err)
	}
	assertNoInstallWrites(t, f)
	secrets, err := f.client.CoreV1().Secrets("a-runs").List(t.Context(), metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 0 {
		t.Fatal("identity published in another installation", err)
	}
}

func addMembershipRelease(t *testing.T, f fixture, manifest string) {
	t.Helper()
	record := &release.Release{Name: "oberth", Namespace: "a", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest}
	if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("a")).Create("sh.helm.release.v1.oberth.v1", record); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceMembershipIncludesNamespacedObjectsAndHistory(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Namespace = "a-runs"
	addMembershipRelease(t, f, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: pipeline\n  namespace: a-runs\n")
	next := &release.Release{Name: "oberth", Namespace: "a", Version: 2, Info: &release.Info{Status: release.StatusFailed}, Config: map[string]any{"pipeline": map[string]any{"namespace": "other-jobs"}}}
	if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("a")).Create("sh.helm.release.v1.oberth.v2", next); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "a/oberth") {
		t.Fatal("older manifest membership was discarded", err)
	}
	assertNoInstallWrites(t, f)
}

func TestUnreadableNamespaceMembershipRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"secret", "manifest", "list"} {
		for _, recheck := range []bool{false, true} {
			t.Run(shape+map[bool]string{true: " recheck"}[recheck], func(t *testing.T) { unreadableMembershipCase(t, shape, recheck) })
		}
	}
}
func unreadableMembershipCase(t *testing.T, shape string, recheck bool) {
	t.Helper()
	f := setup(t)
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	switch shape {
	case "secret":
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.oberth.v1", Namespace: "a", Labels: map[string]string{"owner": "helm", "name": "oberth"}}, Data: map[string][]byte{"release": []byte("unreadable")}}
		if _, err := f.client.CoreV1().Secrets("a").Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	case "manifest":
		addMembershipRelease(t, f, "metadata: [\n")
	case "list":
		addMembershipRelease(t, f, "apiVersion: v1\nkind: List\nitems: [unreadable]\n")
	}
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil {
		t.Fatal("unresolved release membership accepted")
	}
	assertNoInstallWrites(t, f)
}

func TestCustomProductReleaseNamesHoldNamespaces(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"a", "a-runs"} {
		t.Run(server, func(t *testing.T) {
			f := setup(t)
			f.choices.Namespace = server
			record := &release.Release{Name: "custom", Namespace: "a", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Chart: installer.ReleaseChartForTest(f.release), Manifest: "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Namespace\n  metadata:\n    name: a-runs\n"}
			if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("a")).Create("sh.helm.release.v1.custom.v1", record); err != nil {
				t.Fatal(err)
			}
			f.client.ClearActions()
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil || !strings.Contains(err.Error(), "a/custom") {
				t.Fatal("custom release membership ignored", err)
			}
			assertNoInstallWrites(t, f)
		})
	}
}

func TestUnresolvedReleaseIdentityAndAnswersRefuse(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name   string
		change func(*release.Release)
	}{
		{"namespace identity", func(r *release.Release) { r.Namespace = "wrong" }},
		{"unknown chart", func(r *release.Release) { r.Name = "custom" }},
		{"answers", func(r *release.Release) { r.Config = map[string]any{"pipeline": []any{"unreadable"}} }},
		{"namespace name", func(r *release.Release) { r.Manifest = "apiVersion: v1\nkind: Namespace\nmetadata: {}\n" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := setup(t)
			record := &release.Release{Name: "oberth", Namespace: "a", Version: 1, Info: &release.Info{Status: release.StatusDeployed}}
			change.change(record)
			if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets("a")).Create("sh.helm.release.v1.oberth.v1", record); err != nil {
				t.Fatal(err)
			}
			f.client.ClearActions()
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil {
				t.Fatal("unresolved membership accepted")
			}
			assertNoInstallWrites(t, f)
		})
	}
}
