package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Under the nonroot profile the argo-helm chart never renders
// controller.parallelism (configMap.create is false), so the limit must
// travel inside the profile ConfigMap the controller reads (#1517). This
// renders the pinned chart with exactly the installer's profile values and
// limit arguments for --argo-parallelism 6 and checks the controller config.
func TestNonrootProfileRendersArgoParallelism(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; the chart lane runs this render")
	}
	parallelism := 6
	cfg := Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace, ArgoParallelism: &parallelism}
	file, profile, cleanup, err := prepareNonrootArgoValues(t.Context(), cfg, Deps{Output: io.Discard}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if profile.Parallelism != 6 || profile.ConfigMap == argoworkflow.NonrootProfileConfigMap {
		t.Fatalf("profile = %+v", profile)
	}
	cfg.argoProfile = profile
	args := []string{"template", "argo-workflows", "testdata/argo-workflows-1.0.24.tgz", "-n", DefaultArgoNamespace, "--values", file}
	installer := ArgoHelmArgs(cfg)
	for index := 0; index+1 < len(installer); index++ {
		if slices.Contains([]string{"--set", "--set-string", "--set-json"}, installer[index]) {
			args = append(args, installer[index], installer[index+1])
			index++
		}
	}
	var stderr bytes.Buffer
	command := exec.CommandContext(t.Context(), helm, args...)
	command.Stderr = &stderr
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var config, configMapArg string
	for _, document := range strings.Split(string(rendered), "\n---") {
		var object struct {
			Kind     string                `json:"kind"`
			Metadata struct{ Name string } `json:"metadata"`
			Data     map[string]string     `json:"data"`
			Spec     struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `json:"name"`
							Args []string `json:"args"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if yaml.Unmarshal([]byte(document), &object) != nil {
			continue
		}
		switch {
		case object.Kind == "ConfigMap" && object.Metadata.Name == profile.ConfigMap:
			config = object.Data["config"]
		case object.Kind == "Deployment":
			for _, container := range object.Spec.Template.Spec.Containers {
				if index := slices.Index(container.Args, "--configmap"); container.Name == "controller" && index >= 0 && index+1 < len(container.Args) {
					configMapArg = container.Args[index+1]
				}
			}
		}
	}
	if !strings.Contains(config, `"parallelism":6`) || config != profile.Config {
		t.Fatalf("profile ConfigMap config = %q, want %q", config, profile.Config)
	}
	if configMapArg != profile.ConfigMap {
		t.Fatalf("controller reads ConfigMap %q, want the parallelism profile %q", configMapArg, profile.ConfigMap)
	}
	if _, err := argoworkflow.ParseNonrootProfile(profile.ConfigMap, config); err != nil {
		t.Fatalf("rendered profile does not verify: %v", err)
	}
	// The reuse check accepts the stored values this render came from.
	stored := nonrootArgoValues(DefaultArgoNamespace, *profile)
	stored["controller"].(map[string]any)["parallelism"] = float64(6)
	if err := validateNonrootReuse(roundTrip(t, stored), DefaultArgoNamespace); err != nil {
		t.Fatal(err)
	}
	stored["controller"].(map[string]any)["parallelism"] = float64(5)
	if err := validateNonrootReuse(roundTrip(t, stored), DefaultArgoNamespace); err == nil {
		t.Fatal("a profile that disagrees with the stored parallelism was reused")
	}
}

// roundTrip gives values the shape helm get values -o json returns.
func roundTrip(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	encoded, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// --argo-parallelism on a release that runs the nonroot profile keeps the
// profile and moves the limit into it, even without --argo-controller-profile,
// and the Oberth chart follows it without a pinned name.
func TestArgoParallelismOnANonrootReleaseKeepsTheProfile(t *testing.T) {
	var upgrade []string
	deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return json.Marshal(nonrootArgoValues(DefaultArgoNamespace, baseNonrootProfile()))
		case "upgrade":
			upgrade = slices.Clone(args)
		}
		return nil, nil
	}}
	parallelism := 6
	result, err := InstallArgoWorkflows(t.Context(), Config{ArgoParallelism: &parallelism}, deps)
	if err != nil || result.Profile == nil || result.Profile.Parallelism != 6 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !slices.Contains(upgrade, "--values") {
		t.Fatalf("profile values not applied: %v", upgrade)
	}
	// The Oberth chart pins no profile name and clears an earlier pin: the
	// server follows the profile the controller runs.
	values := argoOberthHelmValues(Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, argoProfile: result.Profile}, OpenBaoResult{})
	if !slices.Contains(values, "argo.controllerProfileConfigMap=") || slices.Contains(values, "argo.controllerProfileConfigMap="+result.Profile.ConfigMap) {
		t.Fatalf("Oberth values pin the profile name: %v", values)
	}
}
