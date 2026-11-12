package main

import (
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestChartPersistsAdvertisedClientEndpoint(t *testing.T) {
	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set",
		"image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64),
		"--set", "service.type=NodePort", "--set-string", "clientEndpoint.host=localhost",
		"--set-string", "clientEndpoint.sshPort=32022", "--set-string", "clientEndpoint.httpsPort=32443",
		"--show-only", "templates/service.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render Service: %v\n%s", err, output)
	}
	var service corev1.Service
	if err := yaml.Unmarshal(output, &service); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"oberth.ci/client-host": "localhost", "oberth.ci/client-ssh-port": "32022", "oberth.ci/client-https-port": "32443"} {
		if service.Annotations[key] != value {
			t.Fatalf("advertised endpoint missing: %s", key)
		}
	}
	if service.Spec.Ports[0].NodePort != 30022 || service.Spec.Ports[1].NodePort != 30443 {
		t.Fatal("advertised host ports changed cluster NodePorts")
	}
}
