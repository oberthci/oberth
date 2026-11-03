package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartOmitsRetiredExtendedResourceCapacity(t *testing.T) {
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte("admission:\n  declaredResources:\n    extendedResources:\n      devices.example.test/worker: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "helm", "template", "product", "../../charts/oberth", "-f", values).CombinedOutput()
	if err != nil {
		t.Fatalf("extended resource values: %v\n%s", err, out)
	}
	found := false
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var raw json.RawMessage
		if err = decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			continue
		}
		var kind struct{ Kind string }
		if err = json.Unmarshal(raw, &kind); err != nil {
			t.Fatal(err)
		}
		if kind.Kind != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err = json.Unmarshal(raw, &deployment); err != nil {
			t.Fatal(err)
		}
		found = true
		for _, c := range deployment.Spec.Template.Spec.Containers {
			for _, arg := range c.Args {
				if strings.HasPrefix(arg, "--admission-extended-resources=") {
					t.Fatal("retired global capacity rendered", arg)
				}
			}
		}
	}
	if !found {
		t.Fatal("no deployment checked")
	}
}
