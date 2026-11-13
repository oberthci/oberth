package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Audit actual renders, including optional selector-bearing dependency modes.
// A chart/pin update that adds a policy identity must reserve its key first.
func TestInstalledSelectorsCannotBeAcquiredWithAuthorPodMetadata(t *testing.T) {
	cfg := Config{RekorNamespace: "witness", rekorBaoAddress: "https://openbao.openbao.svc:8200", rekorBaoCA: "public-ca"}
	values := filepath.Join(t.TempDir(), "rekor-values.yaml")
	connection := filepath.Join(t.TempDir(), "rekor-connection.json")
	if err := os.WriteFile(values, []byte(RekorHelmValuesYAML(cfg)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection, []byte(rekorConnectionValues(cfg)), 0600); err != nil {
		t.Fatal(err)
	}
	renders := [][]string{
		{"template", "oberth", "../../charts/oberth", "--set", "metrics.serviceMonitor.enabled=true", "--set", "metrics.serviceMonitor.bearerTokenSecretName=metrics-token", "--set", "metrics.serviceMonitor.bearerTokenSecretKey=token", "--set", "metrics.serviceMonitor.caConfigMapName=metrics-ca", "--set", "metrics.serviceMonitor.caConfigMapKey=ca.crt"},
		{"template", "argo-workflows", "testdata/argo-workflows-1.0.24.tgz", "--set", "crds.install=false", "--set", "controller.metricsConfig.enabled=true", "--set", "controller.metricsConfig.serviceMonitor.enabled=true", "--set", "controller.pdb.enabled=true"},
		{"template", "openbao", "testdata/openbao-0.28.6.tgz", "--set", "server.networkPolicy.enabled=true", "--set", "serverTelemetry.serviceMonitor.enabled=true"},
		{"template", "openbao", "testdata/openbao-0.28.6.tgz", "--set", "server.ha.enabled=true", "--set", "injector.enabled=true", "--set", "injector.networkPolicy.enabled=true", "--set", "serverTelemetry.serviceMonitor.enabled=true"},
		{"template", "rekor", "rekorchart", "-n", "witness", "-f", values, "-f", connection},
	}
	keys := map[string]bool{}
	for _, args := range renders {
		rendered, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render %v: %v\n%s", args, err, rendered)
		}
		decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
		for {
			var object map[string]any
			if err = decoder.Decode(&object); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			auditPodMetadataSelectors(t, object, keys)
		}
	}
	for _, key := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component", "component", "openbao-active", "openbao-internal", "kubernetes.io/metadata.name"} {
		if !keys[key] {
			t.Errorf("selector audit did not exercise %s", key)
		}
	}
}

func auditPodMetadataSelectors(t *testing.T, value any, keys map[string]bool) {
	t.Helper()
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			auditPodMetadataSelectors(t, child, keys)
		}
	case map[string]any:
		for name, child := range value {
			if name == "selector" || name == "podSelector" || name == "namespaceSelector" || name == "labelSelector" || name == "objectSelector" {
				if fields, ok := child.(map[string]any); ok && len(fields) > 0 {
					auditPodMetadataSelector(t, fields, keys)
				}
			}
			auditPodMetadataSelectors(t, child, keys)
		}
	}
}

func auditPodMetadataSelector(t *testing.T, fields map[string]any, keys map[string]bool) {
	t.Helper()
	encoded, _ := json.Marshal(fields)
	var selector metav1.LabelSelector
	if _, ok := fields["matchLabels"]; ok || fields["matchExpressions"] != nil {
		if err := json.Unmarshal(encoded, &selector); err != nil {
			t.Fatal(err)
		}
	} else {
		selector.MatchLabels = map[string]string{}
		for key, value := range fields {
			if text, ok := value.(string); ok {
				selector.MatchLabels[key] = text
			}
		}
	}
	if len(selector.MatchLabels)+len(selector.MatchExpressions) == 0 {
		return
	}
	for key := range selector.MatchLabels {
		keys[key] = true
	}
	for _, expr := range selector.MatchExpressions {
		keys[expr.Key] = true
	}
	for key := range keys {
		for _, annotations := range []bool{false, true} {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "golang:1.26-alpine@sha256:" + strings.Repeat("a", 64)}}}}}
			metadata := &wfv1.Metadata{Labels: map[string]string{key: "other"}}
			if annotations {
				metadata = &wfv1.Metadata{Annotations: metadata.Labels}
			}
			wf.Spec.PodMetadata = metadata
			if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err == nil || (!annotations && !strings.Contains(err.Error(), fmt.Sprintf("[%q] is reserved", key))) {
				t.Fatalf("installed selector key %s is author-settable: %v", key, err)
			}
		}
	}
	compiled, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		t.Fatal(err)
	}
	before := labels.Set{"oberth.ci/run": "run", "oberth.ci/trigger": "ci", "oberth.ci/tier": "ci"}
	after := labels.Set{"oberth.ci/run": "run", "oberth.ci/trigger": "ci", "oberth.ci/tier": "ci", "app": "store", "security": "S2"}
	if compiled.Matches(before) != compiled.Matches(after) {
		t.Fatal("author labels changed installed selector membership", selector)
	}
}
