package installer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
)

func TestPublicPortBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		port int
		url  string
		ok   bool
	}{{"last port", 65535, "https://localhost:65535", true}, {"port above limit", 65536, "https://localhost", false}, {"URL above limit", 30022, "https://localhost:65536", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.choices.SSHPort = tc.port
			f.choices.URL = tc.url
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if (err == nil) != tc.ok {
				t.Fatal(err)
			}
			if p != nil {
				p.Close()
			}
		})
	}
}
func TestPinnedImagesAndInternalSourceService(t *testing.T) {
	t.Parallel()
	f := setup(t)
	prepare(t, f)
	request := f.charts.Previews[0]
	objects := rendered(t, request.Values)
	cm := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if !strings.Contains(cm["artifactDrivers"].(string), "oberthci/oberth-source-plugin:dev") {
		t.Fatal(cm)
	}
	controller := object(t, objects, "Deployment", "oberth-controller-workflow-controller")
	if !strings.Contains(jsonOf(t, controller), "quay.io/argoproj/argoexec:v4.0.8") || !strings.Contains(jsonOf(t, controller), "quay.io/argoproj/workflow-controller:v4.0.8") {
		t.Fatal(controller)
	}
	store := object(t, objects, "StatefulSet", "oberth-store")
	if !strings.Contains(jsonOf(t, store), "docker.io/openbao/openbao:2.7.2") {
		t.Fatal(store)
	}
	service := object(t, objects, "Service", "oberth")
	ports := service["spec"].(map[string]any)["ports"].([]any)
	if len(ports) != 2 {
		t.Fatal("public endpoint count", ports)
	}
	source := object(t, objects, "Service", "oberth-source")
	if source["spec"].(map[string]any)["type"] == "NodePort" {
		t.Fatal("source is public")
	}
}
func TestExplicitZeroAndFalseThroughEngine(t *testing.T) {
	t.Parallel()
	f := setup(t)
	zero, off := 0, false
	f.choices.Parallel = &zero
	f.choices.PodFloor = &off
	f.choices.Egress = &off
	prepare(t, f)
	values := f.charts.Previews[0].Values
	runs, ok := values["runs"].(map[string]any)
	if !ok || runs["parallel"] != 0 {
		t.Fatal("explicit zero lost")
	}
	policies := values["policies"].(map[string]any)
	if policies["floor"] != false || policies["egress"] != false {
		t.Fatal("explicit false lost")
	}
}

func TestKubernetesVersionFloor(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"v1.30.0", "v1.29.9"} {
		t.Run(v, func(t *testing.T) {
			f := setup(t)
			f.client.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: v}
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if (err == nil) != (v == "v1.30.0") {
				t.Fatal(v, err)
			}
			if err != nil && (!strings.Contains(err.Error(), "1.30") || !strings.Contains(err.Error(), "Pod-bound") || !strings.Contains(err.Error(), v) || !strings.Contains(err.Error(), "minimum:")) {
				t.Fatal("version refusal does not explain native execution requirements", err)
			}
			if p != nil {
				p.Close()
			}
		})
	}
}

func TestPlanJSONContainsOnlyPublicReview(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	raw, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(raw), `"cluster_uid":"cluster-one"`) {
		t.Fatal(string(raw), err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), "AGE-SECRET-KEY") {
		t.Fatal("private material in public review")
	}
	for _, line := range p.Review() {
		if strings.Contains(line, "argo-workflows:") || strings.Contains(line, "openbao:") {
			t.Fatal("unresolved alias in review")
		}
	}
}

func TestPinnedServerSettingsRefuseOverrides(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"protocol", "runtime", "public", "listen", "namespace"} {
		t.Run(field, func(t *testing.T) {
			f := setup(t)
			f.choices.Values = map[string]any{"server": map[string]any{field: map[string]any{}}}
			if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
				p.Close()
				t.Fatal("pinned setting overridden")
			}
		})
	}
}

func TestUnwiredWitnessRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Values = map[string]any{"witness": map[string]any{"endpoint": "https://witness.oberth.ci"}}
	if plan, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		plan.Close()
		t.Fatal("unwired witness accepted")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("witness refusal wrote to cluster", action.GetVerb())
		}
	}
}

func TestReviewedValuesCannotMoveThroughPortAliases(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Values = map[string]any{"storage": map[string]any{"tmp": "2Gi"}}
	apply(t, f, prepare(t, f))
	plan := prepare(t, f)
	f.charts.History.Deployed.Values["storage"].(map[string]any)["tmp"] = "1855Mi"
	f.client.ClearActions()
	if _, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil); err == nil {
		t.Fatal("changed values accepted after review")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("changed values caused a write", action)
		}
	}
}

func TestVerifiedReleaseOwnsImageProjection(t *testing.T) {
	t.Parallel()
	files := releaseFixture(t)
	r := loadFixture(t, files)
	files.files["oberth.tgz"].Data[0] ^= 1
	f := setup(t)
	f.release = r
	prepare(t, f)
	if f.charts.Previews[0].Values["imageDigest"] != "sha256:"+strings.Repeat("b", 64) {
		t.Fatal("verified image projection changed")
	}
}
