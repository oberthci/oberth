package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNamespaceRolesCannotOverlap(t *testing.T) {
	t.Parallel()
	for _, tc := range []namespaceRoleCase{
		{"server in pipeline", "a-runs", "a-runs-runs", "a-runs", "a", false},
		{"pipeline in server", "a", "a-runs", "a-runs", "a-runs", false},
		{"explicit server", "second", "first", "first", "first", false},
		{"shared pipeline", "second", "jobs", "jobs", "first", false},
		{"unannotated server", "second", "first", "first", "first", true},
	} {
		for _, recheck := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " recheck"}[recheck], func(t *testing.T) {
				namespaceRoleOverlapCase(t, tc, recheck)
			})
		}
	}
}

func TestOwnPipelineNamespaceCannotChange(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.choices.PipelineNamespace = "other-jobs"
	f.client.ClearActions()
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "pipeline namespace of an existing installation cannot be changed") {
		t.Fatal(err)
	}
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal(a)
		}
	}
}

func TestDisjointInstallationsUninstallReach(t *testing.T) {
	t.Parallel()
	first := renderedIn(t, nil, "a")
	second := renderedIn(t, nil, "second")
	for _, a := range first {
		if a["kind"] != "Namespace" {
			continue
		}
		name := a["metadata"].(map[string]any)["name"]
		for _, b := range second {
			if b["metadata"].(map[string]any)["namespace"] == name {
				t.Fatal("uninstall reaches another installation", b)
			}
		}
	}
}

type namespaceRoleCase struct {
	name, server, pipeline, occupied, owner string
	history                                 bool
}

func namespaceRoleOverlapCase(t *testing.T, tc namespaceRoleCase, recheck bool) {
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: tc.server, PipelineNamespace: tc.pipeline, HTTPSNodePort: 31443, SSHNodePort: 31022}
	var p *installer.Plan
	if recheck {
		p = prepare(t, f)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tc.occupied}}
	if !tc.history {
		ns.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": tc.owner}
	}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if tc.history {
		record := &release.Release{Name: "oberth", Namespace: tc.owner, Version: 1, Info: &release.Info{Status: release.StatusDeployed}}
		if err := installer.ReleaseSecretsForTest(f.client.CoreV1().Secrets(tc.owner)).Create("sh.helm.release.v1.oberth.v1", record); err != nil {
			t.Fatal(err)
		}
	}
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), p, p.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	if err == nil || !strings.Contains(err.Error(), tc.occupied) || !strings.Contains(err.Error(), tc.owner+"/oberth") || !strings.Contains(err.Error(), "Choose another") {
		t.Fatal("namespace ownership not explained", err)
	}
	assertNoInstallWrites(t, f)
}
