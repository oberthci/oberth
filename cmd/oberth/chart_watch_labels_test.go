package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// Strict decoding catches duplicate YAML labels before a permissive decoder
// can overwrite connector identity with the generic server labels (#671).
func TestChartWatchLabelsAreUniqueAndPreserveConnectorIdentity(t *testing.T) {
	const publicCert = "-----BEGIN CERTIFICATE-----\npublic-chart-test-anchor\n-----END CERTIFICATE-----\n"
	values, err := json.Marshal(map[string]any{
		"watchTunnel": map[string]any{"enabled": true, "originCACert": publicCert},
		"secretstore": map[string]any{"caCert": publicCert},
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(t.TempDir(), "public-watch.json")
	if err := os.WriteFile(valuesPath, values, 0600); err != nil {
		t.Fatal(err)
	}
	for _, release := range []string{"oberth", "connector-label-test"} {
		t.Run(release, func(t *testing.T) {
			args := []string{"template", release, "../../charts/oberth", "--namespace", "connector-label-test",
				"--set", "image.ref=example.invalid/oberth@" + goProxyDigest, "--values", valuesPath}
			for _, template := range []string{"cloudflared-watch-sa.yaml", "cloudflared-watch-cm.yaml", "cloudflared-watch-origin-ca.yaml", "cloudflared-watch-deployment.yaml"} {
				args = append(args, "--show-only", "templates/"+template)
			}
			rendered, err := exec.Command("helm", args...).Output()
			if err != nil {
				t.Fatalf("render connector chart: %v", err)
			}
			reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered)))
			seen := map[string]bool{}
			for {
				raw, err := reader.Read()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := sigsyaml.YAMLToJSONStrict(raw)
				if err != nil {
					t.Fatalf("connector YAML contains duplicate or invalid fields: %v", err)
				}
				var object struct {
					Kind     string            `json:"kind"`
					Metadata metav1.ObjectMeta `json:"metadata"`
					Spec     struct {
						Selector *metav1.LabelSelector `json:"selector"`
						Template struct {
							Metadata metav1.ObjectMeta `json:"metadata"`
						} `json:"template"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(body, &object); err != nil {
					t.Fatal(err)
				}
				key := object.Kind + "/" + object.Metadata.Name
				if seen[key] {
					t.Fatalf("duplicate connector object %s", key)
				}
				seen[key] = true
				instance := release
				if object.Kind == "Deployment" {
					instance = "cloudflared-watch-oberth-v2"
					selector := map[string]string{"app.kubernetes.io/name": "cloudflared-watch", "app.kubernetes.io/instance": instance}
					if object.Spec.Selector == nil || !reflect.DeepEqual(object.Spec.Selector.MatchLabels, selector) || len(object.Spec.Selector.MatchExpressions) != 0 {
						t.Fatalf("connector selector changed: %+v", object.Spec.Selector)
					}
					podLabels := map[string]string{"app.kubernetes.io/name": "cloudflared-watch", "app.kubernetes.io/instance": instance, "app.kubernetes.io/component": "tunnel-connector"}
					if !reflect.DeepEqual(object.Spec.Template.Metadata.Labels, podLabels) {
						t.Fatalf("connector pod labels changed: %v", object.Spec.Template.Metadata.Labels)
					}
				}
				labels := object.Metadata.Labels
				if len(labels) != 5 || labels["app.kubernetes.io/name"] != "cloudflared-watch" || labels["app.kubernetes.io/instance"] != instance || labels["app.kubernetes.io/component"] != "tunnel-connector" || labels["app.kubernetes.io/managed-by"] != "Helm" || labels["helm.sh/chart"] == "" || object.Metadata.Namespace != "connector-label-test" {
					t.Fatalf("connector metadata differs for %s: %+v", key, object.Metadata)
				}
			}
			want := map[string]bool{"ServiceAccount/cloudflared-watch": true, "ConfigMap/cloudflared-watch-openbao-ca": true, "ConfigMap/cloudflared-watch-oberth-origin-ca": true, "Deployment/cloudflared-watch-oberth-v2": true}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("connector render objects = %v, want %v", seen, want)
			}
		})
	}
}
