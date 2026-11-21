package installer_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/argoproj/argo-workflows/v4/config"
	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
)

func TestHistoryReadFailureUsesClusterGuidance(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"prepare", "revalidate"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			cause := errors.New("release Secret read: access denied by the cluster")
			var plan *installer.Plan
			if phase == "revalidate" {
				plan = prepare(t, f)
			}
			f.engine.Charts = refusingChart{f.charts, "read", cause}
			actions := len(f.client.Actions())
			var err error
			if plan == nil {
				_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
			} else {
				_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
			}
			var p *installer.Problem
			if !errors.As(err, &p) || p.Code != "cluster" || !errors.Is(err, cause) || !strings.Contains(p.Detail, cause.Error()) {
				t.Fatal("history read lost cluster guidance or its cause", err)
			}
			deltaAssertOnlyClusterReads(t, f, actions)
		})
	}
}

func TestSourceArtifactHasOnlyPublicEnvironment(t *testing.T) {
	t.Parallel()
	values := map[string]any{"server": map[string]any{"runtime": map[string]any{"source": map[string]any{
		"endpoint": "https://source.oberth.ci:8445", "server_name": "source.oberth.ci", "ca_pem": "public-test-ca",
	}}}}
	objects := rendered(t, values)
	data := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	cm := &corev1.ConfigMap{Data: map[string]string{}}
	for key, value := range data {
		cm.Data[key] = value.(string)
	}
	native, err := config.NewController("oberth", cm.Name, nil).Parse(cm)
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.EnvVar{
		{Name: "OBERTH_SOURCE_ENDPOINT", Value: "https://source.oberth.ci:8445"},
		{Name: "OBERTH_SOURCE_SERVER_NAME", Value: "source.oberth.ci"},
		{Name: "OBERTH_SOURCE_CA_PEM", Value: "public-test-ca"},
	}
	if native.Executor == nil || !reflect.DeepEqual(native.Executor.Env, want) || len(native.Executor.EnvFrom)+len(native.Executor.VolumeMounts) != 0 {
		t.Fatal("chart supplies source identity instead of the controller's native Pod token", native.Executor)
	}
	raw := jsonOf(t, objects)
	for _, forbidden := range []string{"OBERTH_SOURCE_TOKEN_FILE", "KUBECONFIG"} {
		if strings.Contains(raw, forbidden) {
			t.Fatal("chart renders a source token or kubeconfig setting", forbidden)
		}
	}
}

func TestServerWireOpaqueSourcePolicyAndTypedRoutes(t *testing.T) {
	t.Parallel()
	base := serverJSON(t, rendered(t, nil))
	runtime := base["runtime"].(map[string]any)
	policy := map[string]any{"origin/team/widget": []string{"origin/team/source"}}
	runtime["source_policy"] = policy
	runtime["release_routes"] = map[string]any{"origin/team/widget": []map[string]string{{"pattern": "v*", "file": ".oberth/release.yaml"}}}
	cfg, err := decodeChartServer([]byte(jsonOf(t, base)))
	if err != nil || string(cfg.Runtime.SourcePolicy) != jsonOf(t, policy) || cfg.Runtime.Routes.Validate() != nil {
		t.Fatal("owning runtime wire contracts differ", err)
	}
	// Raw policy validation belongs to the application after config decoding.
	runtime["source_policy"] = []any{true, 7}
	if _, err := decodeChartServer([]byte(jsonOf(t, base))); err != nil {
		t.Fatal("raw source policy was replaced by a second decoder", err)
	}
	runtime["source_policy"] = nil
	if _, err := decodeChartServer([]byte(jsonOf(t, base))); err != nil {
		t.Fatal("raw null policy differs from the runtime type", err)
	}
	delete(runtime, "source_policy")
	if _, err := decodeChartServer([]byte(jsonOf(t, base))); err == nil {
		t.Fatal("explicit source policy field missing")
	}
}
