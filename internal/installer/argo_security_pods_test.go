package installer

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Render the actual pinned native chart and evaluate every Pod template against
// the actual namespace CEL. The controller and an enabled server are included;
// no synthetic security context is added to make their rendered Pods pass.
func TestArgoSecurityEveryRenderedPodPassesPolicy(t *testing.T) {
	evaluate := argoPodPolicyEvaluator(t)
	for _, server := range []bool{false, true} {
		t.Run(map[bool]string{false: "controller", true: "controller-and-server"}[server], func(t *testing.T) {
			flags := ArgoHelmArgs(Config{})
			// Exercise the optional native server in the same namespace, while ordinary
			// managed installs keep the installer's server.enabled=false setting.
			if server {
				flags = append(flags, "--set", "server.enabled=true")
			}
			objects := renderArgoHelmObjects(t, flags, map[string]any{})
			count := 0
			for _, object := range objects {
				var template any
				switch object["kind"] {
				case "Deployment", "StatefulSet", "DaemonSet", "Job":
					template = valueAt(object, "spec", "template")
				case "CronJob":
					template = valueAt(object, "spec", "jobTemplate", "spec", "template")
				case "Pod":
					template = object
				default:
					continue
				}
				namespace, _ := valueAt(object, "metadata", "namespace").(string)
				if namespace != "" && namespace != DefaultArgoNamespace {
					continue
				}
				encoded, err := json.Marshal(template)
				if err != nil {
					t.Fatal(err)
				}
				var pod corev1.Pod
				if err := json.Unmarshal(encoded, &pod); err != nil {
					t.Fatal(err)
				}
				if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Fatalf("native %v %v lacks Pod RuntimeDefault default", object["kind"], valueAt(object, "metadata", "name"))
				}
				if failures := evaluate(pod); len(failures) != 0 {
					t.Fatalf("native %v %v refused: %v", object["kind"], valueAt(object, "metadata", "name"), failures)
				}
				count++
			}
			expected := 1
			if server {
				expected = 2
			}
			if count != expected {
				t.Fatalf("evaluated %d Pods, want %d", count, expected)
			}
		})
	}
}

func argoPodPolicyEvaluator(t *testing.T) func(corev1.Pod) []string {
	t.Helper()
	raw, err := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64), "--show-only", "templates/argo-pod-policy.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("policy render: %v: %s", err, raw)
	}
	var policy admissionv1.ValidatingAdmissionPolicy
	if err := yaml.Unmarshal([]byte(strings.Split(string(raw), "\n---")[0]), &policy); err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("variables", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	variables := []cel.Program{}
	checks := []cel.Program{}
	for _, v := range policy.Spec.Variables {
		variables = append(variables, compile(v.Expression))
	}
	for _, v := range policy.Spec.Validations {
		checks = append(checks, compile(v.Expression))
	}
	return func(pod corev1.Pod) []string {
		encoded, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		var input map[string]any
		if err := json.Unmarshal(encoded, &input); err != nil {
			t.Fatal(err)
		}
		vars := map[string]any{}
		activation := map[string]any{"object": input, "variables": vars}
		for i, program := range variables {
			value, _, err := program.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			vars[policy.Spec.Variables[i].Name] = value
		}
		var failures []string
		for i, program := range checks {
			value, _, err := program.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			if value != types.True {
				failures = append(failures, policy.Spec.Validations[i].Message)
			}
		}
		return failures
	}
}
