package argojob_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
	core "k8s.io/api/core/v1"
	psaapi "k8s.io/pod-security-admission/api"
	psapolicy "k8s.io/pod-security-admission/policy"
)

func TestSecretHelperNativePods(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for _, kind := range []string{"container", "script", "container-set", "init", "defaults", "inline"} {
		w := restrictedSecretHelperWorkflow(kind)
		grant := selectedGrant()
		if kind == "inline" {
			grant.Step = "*"
		}
		p, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(p.View().Workflow)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(directory, kind+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := holdControllerFixture.Command(t.Context(), t, "-test.count=1", "-test.timeout=120s", "-test.run=^TestOberthSecretHelperPods$")
	command.Env = append(os.Environ(), "OBERTH_SECRET_HELPER_CASES="+directory)
	if raw, err := controllertest.Output(command); err != nil {
		t.Fatalf("native secret helper Pods: %v\n%s", err, raw)
	}
	checkRestrictedHelperPods(t, directory)
}

func restrictedSecretHelperWorkflow(kind string) *wf.Workflow {
	w := secretHelperWorkflow(kind)
	w.Spec.SecurityContext = &core.PodSecurityContext{RunAsNonRoot: new(true)}
	var templates []*wf.Template
	for i := range w.Spec.Templates {
		templates = append(templates, &w.Spec.Templates[i])
	}
	if w.Spec.TemplateDefaults != nil {
		templates = append(templates, w.Spec.TemplateDefaults)
	}
	for _, template := range templates {
		for i := range template.InitContainers {
			template.InitContainers[i].SecurityContext = common.MinimalCtrSC()
		}
	}
	return w
}

func checkRestrictedHelperPods(t *testing.T, directory string) {
	t.Helper()
	evaluator, err := psapolicy.NewEvaluator(psapolicy.DefaultChecks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.pods.json"))
	if err != nil || len(files) != 6 {
		t.Fatal("missing rendered Pod cases", files, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var pods core.PodList
		if err = json.Unmarshal(raw, &pods); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) != 2 {
			t.Fatal("missing declaring or ordinary Pod", file)
		}
		for i := range pods.Items {
			if slices.ContainsFunc(pods.Items[i].Spec.InitContainers, func(c core.Container) bool { return c.Name == "oberth-secret-install" }) {
				checkRestrictedHelperPod(t, evaluator, &pods.Items[i])
			}
		}
	}
}

func restrictedHelperResult(evaluator psapolicy.Evaluator, pod *core.Pod) psapolicy.AggregateCheckResult {
	level := psaapi.LevelVersion{Level: psaapi.LevelRestricted, Version: psaapi.MajorMinorVersion(1, 35)}
	return psapolicy.AggregateCheckResults(evaluator.EvaluatePod(level, &pod.ObjectMeta, &pod.Spec))
}

func checkRestrictedHelperPod(t *testing.T, evaluator psapolicy.Evaluator, pod *core.Pod) {
	t.Helper()
	result := restrictedHelperResult(evaluator, pod)
	if !result.Allowed {
		t.Fatal("rendered Pod violates restricted policy", pod.Name, result.ForbiddenDetail())
	}
	for i, init := range pod.Spec.InitContainers {
		if init.Name != "oberth-secret-install" {
			continue
		}
		for _, field := range []string{"escalation", "capabilities", "seccomp"} {
			control := pod.DeepCopy()
			security := control.Spec.InitContainers[i].SecurityContext
			switch field {
			case "escalation":
				security.AllowPrivilegeEscalation = nil
			case "capabilities":
				security.Capabilities = nil
			case "seccomp":
				security.SeccompProfile = nil
			}
			denied := restrictedHelperResult(evaluator, control)
			if denied.Allowed || !strings.Contains(denied.ForbiddenDetail(), init.Name) {
				t.Fatal("restricted policy accepted helper without its floor", field, denied.ForbiddenDetail())
			}
		}
	}
}
