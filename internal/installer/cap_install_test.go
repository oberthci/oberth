package installer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
	corev1 "k8s.io/api/core/v1"
)

func TestCapOnlyInstallKeepsServerAndStoreTemplates(t *testing.T) {
	native := controllertest.New(map[string]string{"oberth_cap_reload_test.go": "testdata/cap_reload_controller_test.go"})
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, origins := range []string{"recorded", "absent", "sparse"} {
		t.Run(origins, func(t *testing.T) {
			requests := capInstalls(t, origins)
			configs := assertCapTemplates(t, requests)
			runNativeCapReload(t, native, configs)
		})
	}
}

func capInstalls(t *testing.T, origins string) [2]installer.ChartRequest {
	t.Helper()
	f := setup(t)
	parallel := 6
	f.choices.Parallel = &parallel
	f.choices.SourcePins = map[string][]string{}
	for i := range 33 {
		f.choices.SourcePins["origin/team/app"] = append(f.choices.SourcePins["origin/team/app"], fmt.Sprintf("origin/team/lib%d", i))
	}
	first := apply(t, f, prepare(t, f))
	before := f.charts.Applies[len(f.charts.Applies)-1]
	var history installer.History
	if err := json.Unmarshal([]byte(jsonOf(t, f.charts.History)), &history); err != nil {
		t.Fatal(err)
	}
	// Recorded chart inputs define the installed templates, including sparse metadata.
	for _, values := range []map[string]any{history.Deployed.Values, history.Latest.Values} {
		switch origins {
		case "absent":
			delete(values, "statusOrigins")
		case "sparse":
			values["statusOrigins"] = map[string]any{"server URL": "", "source pins": "chosen"}
		}
	}
	f.charts.History = history
	before.Values = history.Deployed.Values
	f.engine.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }
	parallel = 8
	f.choices = installer.Choices{Context: "test", Parallel: &parallel}
	second := apply(t, f, prepare(t, f))
	if first.Profile.AdminCert == second.Profile.AdminCert {
		t.Fatal("test did not exercise certificate renewal")
	}
	return [2]installer.ChartRequest{before, f.charts.Applies[len(f.charts.Applies)-1]}
}

type capSnapshot struct {
	namespace  string
	resources  map[string]string
	controller corev1.ConfigMap
}

func capChartSnapshot(t *testing.T, request installer.ChartRequest) capSnapshot {
	t.Helper()
	snapshot := capSnapshot{namespace: request.Namespace, resources: map[string]string{}}
	for _, o := range renderedIn(t, request.Values, request.Namespace) {
		metadata := o["metadata"].(map[string]any)
		if o["kind"] == "ConfigMap" && metadata["name"] == "oberth-controller-config" {
			decodeObject(t, o, &snapshot.controller)
			continue
		}
		id := fmt.Sprintf("%v/%v/%v", o["kind"], metadata["namespace"], metadata["name"])
		if _, exists := snapshot.resources[id]; exists {
			t.Fatal("duplicate rendered resource", id)
		}
		snapshot.resources[id] = jsonOf(t, o)
	}
	return snapshot
}

func assertCapResources(t *testing.T, before, after capSnapshot) {
	t.Helper()
	for _, item := range [][2]string{{"Deployment", "oberth"}, {"StatefulSet", "oberth-store"}, {"Deployment", installer.ControllerDeployment}} {
		id := item[0] + "/" + before.namespace + "/" + item[1]
		if before.resources[id] == "" || after.resources[id] == "" {
			t.Fatal("cap comparison omitted a control Pod template", id)
		}
	}
	if len(before.resources) != len(after.resources) {
		t.Fatal("cap-only install changed the resource inventory")
	}
	for id, value := range before.resources {
		if after.resources[id] != value {
			t.Fatal("cap-only install changed resource", id)
		}
	}
}

func assertCapTemplates(t *testing.T, requests [2]installer.ChartRequest) [2]corev1.ConfigMap {
	t.Helper()
	before, after := capChartSnapshot(t, requests[0]), capChartSnapshot(t, requests[1])
	assertCapResources(t, before, after)
	configs := [2]corev1.ConfigMap{before.controller, after.controller}
	if configs[0].Data["parallelism"] != "6" || configs[1].Data["parallelism"] != "8" {
		t.Fatal("run cap did not reach the controller", configs[0].Data["parallelism"], configs[1].Data["parallelism"])
	}
	if configs[1].Annotations["oberth.ci/parallel-origin"] != "chosen" {
		t.Fatal("explicit cap lost its chosen origin")
	}
	var rest [2]string
	for i := range configs {
		other := configs[i].DeepCopy()
		delete(other.Data, "parallelism")
		delete(other.Annotations, "oberth.ci/parallel-origin")
		rest[i] = jsonOf(t, other)
	}
	if rest[0] != rest[1] {
		t.Fatal("cap-only install changed other controller configuration")
	}
	return configs
}

func runNativeCapReload(t *testing.T, native *controllertest.Fixture, configs [2]corev1.ConfigMap) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cap-configs.json")
	if err := os.WriteFile(path, []byte(jsonOf(t, configs)), 0600); err != nil {
		t.Fatal(err)
	}
	command := native.Command(t.Context(), t, "-test.count=1", "-test.timeout=120s", "-test.v", "-test.run=^TestOberthCapConfigReload$")
	command.Env = append(os.Environ(), "OBERTH_CAP_CONFIGS="+path)
	raw, err := controllertest.Output(command)
	if err != nil {
		t.Fatalf("native controller cap reload: %v\n%s", err, raw)
	}
	t.Log(string(raw))
}
