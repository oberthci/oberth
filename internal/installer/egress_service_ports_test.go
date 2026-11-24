package installer_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestServerServiceTargetPortsDriveEgress(t *testing.T) {
	chart, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, file := range chart.Raw {
		if file.Name == "templates/_services.tpl" {
			next := bytes.ReplaceAll(file.Data, []byte("targetPort: ssh"), []byte("targetPort: 2299"))
			changed = !bytes.Equal(next, file.Data)
			file.Data = next
		}
	}
	if !changed {
		t.Fatal("server Service port fixture was not changed")
	}
	files, err := installer.Render(chart, networkValues(false), "oberth")
	if err != nil {
		t.Fatal(err)
	}
	network := networkFromFiles(t, files)
	target := networkTarget{ip: "198.18.0.10", namespace: "oberth", labels: map[string]string{"app": "oberth"}, ports: []corev1.ContainerPort{{Name: "ssh", ContainerPort: 2222}}}
	if !policyAllows(network, target, corev1.ProtocolTCP, 2299) || policyAllows(network, target, corev1.ProtocolTCP, 2222) {
		t.Fatal("egress ports did not follow the server Service")
	}
}

func networkFromFiles(t *testing.T, files map[string]string) networkingv1.NetworkPolicy {
	t.Helper()
	for name, raw := range files {
		if !strings.HasSuffix(name, "/pipeline.yaml") {
			continue
		}
		decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(raw), 4096)
		for {
			var object map[string]any
			err := decoder.Decode(&object)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if object["kind"] == "NetworkPolicy" {
				var network networkingv1.NetworkPolicy
				decodeObject(t, object, &network)
				return network
			}
		}
	}
	t.Fatal("run egress policy missing")
	return networkingv1.NetworkPolicy{}
}
