package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func rekorTestKey(t *testing.T) map[string]any {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "ecdsa-p256", "derived": false, "exportable": false, "allow_plaintext_backup": false, "latest_version": float64(1), "keys": map[string]any{"1": map[string]any{"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}
}

func TestRekorTransitRefusesUnsafeOrRotatedKeys(t *testing.T) {
	for _, field := range []string{"derived", "exportable", "allow_plaintext_backup", "latest_version", "type", "keys"} {
		t.Run(field, func(t *testing.T) {
			key := rekorTestKey(t)
			key[field] = true
			if _, err := rekorTransitPublicKey(key); err == nil {
				t.Fatal("unsafe key accepted")
			}
		})
	}
}

func TestRekorBaoProvisioningPreservesCredentials(t *testing.T) {
	ctx := context.Background()
	key := rekorTestKey(t)
	state := map[string]map[string]any{"oberth-transit/keys/rekor-rekor": key}
	writes := map[string]int{}
	const token = "test-administrative-token"
	deps := Deps{KubeClient: fake.NewClientset(), Output: io.Discard, RunHelm: func(context.Context, []string) ([]byte, error) { return []byte("[]"), nil }}
	deps.RunCommand = func(_ context.Context, input []byte, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), token) {
			t.Fatal("token in argv")
		}
		command, authenticated, err := stripKubectlBaoPlumbing(args)
		if err != nil || !authenticated {
			t.Fatalf("bad authenticated command: %v", err)
		}
		if !strings.HasPrefix(string(input), token+"\n") {
			t.Fatal("missing token stdin")
		}
		fields := strings.Fields(command)
		switch fields[0] {
		case "read":
			data, ok := state[fields[2]]
			if !ok {
				return []byte("No value found"), errors.New("missing")
			}
			return json.Marshal(map[string]any{"data": data})
		case "write":
			var data map[string]any
			if err := json.Unmarshal(input[len(token)+1:], &data); err != nil {
				t.Fatal(err)
			}
			path := fields[1]
			writes[path]++
			if strings.Contains(path, "/data/") {
				if data["options"].(map[string]any)["cas"] != float64(0) {
					t.Fatal("credential write lacks CAS")
				}
				state[path] = data
			}
			return nil, nil
		case "policy":
			return nil, nil
		}
		t.Fatalf("unexpected command %s", command)
		return nil, nil
	}
	store := newOpenBaoExec(deps, "openbao", "openbao-0")
	first, err := configureRekorBao(ctx, Config{}, deps, store, token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := configureRekorBao(ctx, Config{}, deps, store, token)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("public identity changed")
	}
	for _, name := range []string{"rekor-db", "rekor-db-root"} {
		if writes["oberth/data/identities/rekor/"+name] != 1 {
			t.Fatal("database credential overwritten")
		}
	}
	for _, action := range deps.KubeClient.(*fake.Clientset).Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("unexpected Kubernetes mutation: %v", action)
		}
	}
	delete(state, "oberth/data/identities/rekor/rekor-db")
	state["oberth/metadata/identities/rekor/rekor-db"] = map[string]any{"current_version": float64(1)}
	if _, err := configureRekorBao(ctx, Config{}, deps, store, token); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("deleted credential not protected: %v", err)
	}
}

func TestRekorBaoRejectsLegacyBeforeProvisioning(t *testing.T) {
	deps := Deps{KubeClient: fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rekor-signer", Namespace: "rekor"}})}
	if _, err := configureRekorBao(context.Background(), Config{}, deps, openBaoExec{}, "unused"); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy identity not protected: %v", err)
	}
}

func TestRekorChartHasMemoryOnlyCredentials(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm unavailable")
	}
	dir, err := extractRekorChart()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := Config{RekorNamespace: "witness", rekorBaoAddress: "https://openbao.openbao.svc:8200", rekorBaoCA: "public-ca"}
	values := filepath.Join(dir, "values.yaml")
	connection := filepath.Join(dir, "connection.json")
	if err := os.WriteFile(values, []byte(RekorHelmValuesYAML(cfg)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection, []byte(rekorConnectionValues(cfg)), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(context.Background(), helm, "template", "rekor", filepath.Join(dir, "rekorchart"), "-n", "witness", "-f", values, "-f", connection).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, output)
	}
	for _, forbidden := range []string{"kind: Secret\n", "secretKeyRef:", "secretName:", "PRIVATE KEY", "$(MYSQL_PASSWORD)"} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("render contains %q", forbidden)
		}
	}
	workloads := 0
	for _, doc := range strings.Split(string(output), "\n---") {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
			t.Fatal(err)
		}
		var pod corev1.PodSpec
		switch kind.Kind {
		case "Deployment":
			var d appsv1.Deployment
			if err := yaml.UnmarshalStrict([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			pod = d.Spec.Template.Spec
		case "Job":
			var j batchv1.Job
			if err := yaml.UnmarshalStrict([]byte(doc), &j); err != nil {
				t.Fatal(err)
			}
			pod = j.Spec.Template.Spec
		default:
			continue
		}
		if !strings.HasPrefix(pod.ServiceAccountName, "rekor-bao-") {
			continue
		}
		workloads++
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Fatal("implicit API token")
		}
		found := false
		for _, v := range pod.Volumes {
			if v.Name == "bao-credentials" {
				found = true
				if v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory {
					t.Fatal("credential volume is not tmpfs")
				}
			}
		}
		if !found {
			t.Fatal("missing credential volume")
		}
		for _, c := range pod.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name == "bao-login" {
					t.Fatal("application mounts authentication JWT")
				}
			}
		}
		if len(pod.InitContainers) < 2 || pod.InitContainers[1].RestartPolicy == nil || *pod.InitContainers[1].RestartPolicy != corev1.ContainerRestartPolicyAlways {
			t.Fatal("renewing authentication sidecar absent")
		}
	}
	if workloads != 5 {
		t.Fatalf("checked %d workloads, want 5", workloads)
	}
}
