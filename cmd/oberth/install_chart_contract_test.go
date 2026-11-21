package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
	"helm.sh/helm/v3/pkg/chart/loader"
	core "k8s.io/api/core/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartSchemaMatchesExecutable(t *testing.T) {
	t.Parallel()
	chart, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(chart.Values["targetSchema"]) != fmt.Sprint(store.Schema) {
		t.Fatal("chart and executable database schemas differ", chart.Values["targetSchema"], store.Schema)
	}
}

func TestRenderedChartStrictConfigAndMountedFiles(t *testing.T) {
	objects := renderedContractObjects(t)
	var configuration core.ConfigMap
	if err := json.Unmarshal(objects["ConfigMap/oberth-server"], &configuration); err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig([]byte(configuration.Data["server.json"]))
	if err != nil {
		t.Fatal("rendered server configuration does not load", err)
	}
	var deployment struct {
		Spec struct{ Template core.PodTemplateSpec }
	}
	if err = json.Unmarshal(objects["Deployment/oberth"], &deployment); err != nil {
		t.Fatal(err)
	}
	store := storeConfiguration(cfg)
	if store.ServerAccount != deployment.Spec.Template.Spec.ServiceAccountName || store.StepRole != "oberth-step" {
		t.Fatal("store account or fixed step login role differs from installation contract", store.ServerAccount, store.StepRole)
	}
	mounted := contractMountedFiles(deployment.Spec.Template.Spec)
	files := reflect.ValueOf(cfg.Files)
	typ := files.Type()
	for i := 0; i < files.NumField(); i++ {
		path := files.Field(i).String()
		key := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if path == "" {
			t.Errorf("files.%s: no configured path", key)
		} else if !mounted[path] {
			t.Errorf("files.%s: %s is not a rendered mounted file", key, path)
		}
	}
	if cfg.SecretStore.Audience != "contract-varying-audience" {
		t.Errorf("store audience: got %q", cfg.SecretStore.Audience)
	}
	if _, err = decodeRuntime(cfg.Runtime); err != nil {
		t.Errorf("strict runtime: %v", err)
	}
}

func renderedContractObjects(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	chart, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"secretStore": map[string]any{"audience": "contract-varying-audience"}}
	manifests, err := installer.Render(chart, values, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]json.RawMessage{}
	for name, text := range manifests {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		readContractObjects(t, text, result)
	}
	return result
}

func readContractObjects(t *testing.T, text string, result map[string]json.RawMessage) {
	t.Helper()
	decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var identity struct {
			Kind     string
			Metadata struct{ Name string }
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			t.Fatal(err)
		}
		result[identity.Kind+"/"+identity.Metadata.Name] = raw
	}
}

func contractMountedFiles(pod core.PodSpec) map[string]bool {
	result := map[string]bool{}
	volumes := map[string]core.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	for _, container := range pod.Containers {
		if container.Name != "server" {
			continue
		}
		for _, mount := range container.VolumeMounts {
			v := volumes[mount.Name]
			items := contractVolumeItems(v)
			for _, item := range items {
				switch mount.SubPath {
				case "":
					result[filepath.Join(mount.MountPath, item.Path)] = true
				case item.Path:
					result[mount.MountPath] = true
				}
			}
		}
	}
	return result
}

func contractVolumeItems(v core.Volume) []core.KeyToPath {
	var items []core.KeyToPath
	if v.Secret != nil {
		items = v.Secret.Items
	}
	if v.ConfigMap != nil {
		items = v.ConfigMap.Items
	}
	if v.Projected != nil {
		for _, source := range v.Projected.Sources {
			if source.ServiceAccountToken != nil {
				items = append(items, core.KeyToPath{Path: source.ServiceAccountToken.Path})
			}
		}
	}
	return items
}
