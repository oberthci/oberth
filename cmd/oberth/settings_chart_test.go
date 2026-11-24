package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	core "k8s.io/api/core/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRenderedDefaultChartAndEveryExplicitOriginAgree(t *testing.T) {
	for _, source := range []string{"default", "chosen"} {
		origins := map[string]any{}
		if source == "chosen" {
			for _, name := range apicontract.InstallationChoiceNames() {
				origins[name] = source
			}
		}
		rows := renderedStatusSettings(t, origins)
		byName := map[string]apicontract.EffectiveSetting{}
		for _, row := range rows {
			byName[row.Name] = row
			if source == "default" && row.Name != "store state" && row.Source != "default" {
				t.Fatal("rendered chart default marked as chosen", row)
			}
		}
		for _, name := range apicontract.InstallationChoiceNames() {
			if byName[name].Source != source {
				t.Fatal("choice name or origin drifted from chart", name, byName[name], source)
			}
		}
	}
}

func renderedStatusSettings(t *testing.T, origins map[string]any) []apicontract.EffectiveSetting {
	t.Helper()
	ch, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	files, err := installer.Render(ch, map[string]any{"statusOrigins": origins}, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	maps := renderedStatusMaps(t, files)
	cfg, err := decodeConfig([]byte(maps["oberth-server"].Data["server.json"]))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := decodeRuntime(cfg.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	c := &composition{config: cfg, settings: runtime}
	a := serverAdministration{&serverOwners{c: c, settings: runtime, cluster: clusterClients{Kube: fake.NewClientset(maps["oberth-controller-config"])}}}
	var cap settingsRows
	cap.runCap(t.Context(), a.owner)
	return a.effectiveSettings(t.Context(), []apicontract.PartStatus{{Name: "secret store", Ready: true}}, cap.values)
}

func renderedStatusMaps(t *testing.T, files map[string]string) map[string]*core.ConfigMap {
	t.Helper()
	maps := map[string]*core.ConfigMap{}
	for name, text := range files {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
		for {
			var object core.ConfigMap
			err := decoder.Decode(&object)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if object.Kind == "ConfigMap" {
				maps[object.Name] = &object
			}
		}
	}
	return maps
}
