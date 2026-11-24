package installer_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPipelinePodSecurityRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"", "privileged", "Baseline", "unknown"} {
		for _, phase := range []string{"prepare", "apply", "before-write"} {
			t.Run(level+"/"+phase, func(t *testing.T) { podSecurityRefusalCase(t, level, phase) })
		}
	}
}

func podSecurityRefusalCase(t *testing.T, level, phase string) {
	t.Helper()
	f := setup(t)
	var plan *installer.Plan
	if phase != "prepare" {
		plan = prepare(t, f)
	}
	if phase != "before-write" {
		pipelineNamespaceLevel(t, f, level)
	}
	var err error
	if phase == "prepare" {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	} else {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), func(p installer.Phase) error {
			if phase == "before-write" && p == installer.ReleaseChecked {
				pipelineNamespaceLevel(t, f, level)
			}
			return nil
		})
	}
	assertPodSecurityRefusal(t, err)
	assertNoInstallWrites(t, f)
}

func TestPipelinePodSecurityUpgradeRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	for _, atApply := range []bool{false, true} {
		t.Run(boolText(atApply), func(t *testing.T) { upgradePodSecurityRefusalCase(t, atApply) })
	}
}

func upgradePodSecurityRefusalCase(t *testing.T, atApply bool) {
	t.Helper()
	f, m := maintenanceFixture(t)
	var plan *installer.UpgradePlan
	if atApply {
		var err error
		plan, err = m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Plan.Close()
	}
	pipelineNamespaceLevel(t, f, "privileged")
	before := localFiles(t, f.dir)
	var err error
	if atApply {
		_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	} else {
		_, err = m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	}
	assertPodSecurityRefusal(t, err)
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal("refusal wrote", a)
		}
	}
	if len(f.charts.Applies) != 1 || !reflect.DeepEqual(before, localFiles(t, f.dir)) {
		t.Fatal("refusal changed installation")
	}
}

func TestPipelinePodSecurityReviewAndVerification(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"", "baseline", "restricted"} {
		t.Run(level, func(t *testing.T) {
			f := setup(t)
			want := "baseline (new)"
			if level != "" {
				pipelineNamespaceLevel(t, f, level)
				want = level
			}
			plan := prepare(t, f)
			if !strings.Contains(strings.Join(plan.Review(), "\n"), "Pod Security: restricted (new) / "+want) {
				t.Fatal(plan.Review())
			}
			if level != "" {
				if err := installer.VerifyPipelineNamespace(t.Context(), f.client.CoreV1().Namespaces(), "oberth-runs"); err != nil {
					t.Fatal(err)
				}
			}
			assertNoInstallWrites(t, f)
		})
	}
	f := setup(t)
	pipelineNamespaceLevel(t, f, "")
	assertPodSecurityRefusal(t, installer.VerifyPipelineNamespace(t.Context(), f.client.CoreV1().Namespaces(), "oberth-runs"))
	assertNoInstallWrites(t, f)
}

func pipelineNamespaceLevel(t *testing.T, f fixture, level string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-runs", Labels: map[string]string{"pod-security.kubernetes.io/enforce": level}}}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
}

func assertPodSecurityRefusal(t *testing.T, err error) {
	t.Helper()
	var p *installer.Problem
	if !errors.As(err, &p) || p.Code != "pipeline-security" || !strings.Contains(p.Detail, "oberth-runs") {
		t.Fatal("missing namespace refusal", err)
	}
	if len(p.Reason) > 80 || len(p.Remedy) > 120 || !strings.Contains(p.Remedy, "pod-security.kubernetes.io/enforce=baseline") {
		t.Fatal(p)
	}
}

func localFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = string(data)
	}
	return result
}
