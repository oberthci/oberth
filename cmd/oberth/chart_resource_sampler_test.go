package main

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartResourceSamplerOptInRestrictedToServerAndExactNodes(t *testing.T) {
	render := func(enabled bool) []byte {
		t.Helper()
		args := []string{"template", "oberth", "../../charts/oberth", "--namespace", "server", "--set", "image.ref=example.invalid/oberth@" + testImageDigest}
		if enabled {
			args = append(args, "--set", "argo.resourceSampler.enabled=true", "--set", "argo.resourceSampler.nodes.node-a=https://node-a.example:10250", "--set-string", "argo.resourceSampler.caCert=public-test-ca")
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render %v %s", err, out)
		}
		return out
	}
	if raw := render(false); bytes.Contains(raw, []byte("nodes/metrics")) || bytes.Contains(raw, []byte("--argo-resource-sampler-config")) {
		t.Fatal("sampler enabled by default")
	}
	raw := render(true)
	if !bytes.Contains(raw, []byte("--argo-resource-sampler-config=/etc/oberth/resource-sampler/config.json")) || !bytes.Contains(raw, []byte("mountPath: /etc/oberth/resource-sampler")) || !bytes.Contains(raw, []byte("checksum/resource-sampler:")) {
		t.Fatal("enabled sampler not wired into server")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var role, binding chartRBACDoc
	for {
		var doc chartRBACDoc
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(doc.Metadata.Name, "resource-sampler") {
			switch doc.Kind {
			case "ClusterRole":
				role = doc
			case "ClusterRoleBinding":
				binding = doc
			}
		}
	}
	want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"nodes/stats", "nodes/metrics"}, "resourceNames": []any{"node-a"}, "verbs": []any{"get"}}}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("sampler authority %#v", role.Rules)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0]["name"] != "oberth" || binding.Subjects[0]["namespace"] != "server" || binding.RoleRef["name"] != role.Metadata.Name {
		t.Fatalf("sampler bound outside server %#v", binding)
	}
	// Inspect the sampler objects themselves: labels elsewhere in the rendered
	// chart must not mask an unlabeled ClusterRole or binding.
	decoder = yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	objects := 0
	for {
		var doc struct {
			Kind     string
			Metadata struct {
				Name   string
				Labels map[string]string
			}
			Data map[string]string
		}
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(doc.Metadata.Name, "resource-sampler") {
			continue
		}
		objects++
		if doc.Metadata.Labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Fatalf("sampler %s missing chart labels", doc.Kind)
		}
		if doc.Kind == "ConfigMap" && !strings.Contains(doc.Data["config.json"], `"interval":"15s"`) {
			t.Fatal("tuxbox cadence is not 15s")
		}
	}
	if objects != 3 {
		t.Fatalf("sampler objects %d", objects)
	}
}
func TestChartResourceSamplerRequiresEndpointAndCA(t *testing.T) {
	for _, extra := range [][]string{nil, {"--set", "argo.resourceSampler.nodes.node-a=https://node-a.example:10250"}} {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest, "--set", "argo.resourceSampler.enabled=true"}
		args = append(args, extra...)
		if raw, err := exec.Command("helm", args...).CombinedOutput(); err == nil || !bytes.Contains(raw, []byte("argo.resourceSampler")) {
			t.Fatalf("incomplete sampler accepted: %v %s", err, raw)
		}
	}
}
