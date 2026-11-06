package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Exercise --values, the CLI's UseNumber/JSON migration snapshot, real Helm
// upgrade serialization, and reuse of that serialized release. No cluster writes.
func TestUpgradeIntegerFlagsThroughHelmRoundTrip(t *testing.T) {
	requireHelmForUpgradeTest(t)
	const numericValues = `{"admission":{"diskFloorBytes":21474836480},"kubernetesClient":{"burst":1000000,"qps":50.5},"argo":{"workflowTTL":2592000},"retention":{"logs":{"limitBytes":21474836480,"budgetBytes":21474836480},"artifacts":{"limitBytes":21474836480,"budgetBytes":21474836480},"keepNewestPerRef":0},"trustedTests":{"runtime":"generic-capability","kvmSupplementalGroup":2147483647,"recipes":[{"suite_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","guest_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","harness_image":"example.invalid/harness@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","suite_url":"https://example.invalid/suite","guest_url":"https://example.invalid/guest"}]}}`
	expected := map[string]int64{
		"admission-disk-floor-bytes": 21474836480, "kubernetes-client-burst": 1000000,
		"argo-workflow-ttl":   2592000,
		"run-log-limit-bytes": 21474836480, "logs-budget-bytes": 21474836480,
		"artifacts-limit-bytes": 21474836480, "artifacts-budget-bytes": 21474836480,
		"retention-keep-newest": 0, "max-concurrent-jobs": 3,
	}
	for _, route := range []string{"values-file", "stored-migration"} {
		t.Run(route, func(t *testing.T) {
			dir := t.TempDir()
			history := filepath.Join(dir, "release.json")
			config := map[string]any{}
			if route == "stored-migration" {
				decoder := json.NewDecoder(strings.NewReader(numericValues))
				decoder.UseNumber()
				if err := decoder.Decode(&config); err != nil {
					t.Fatal(err)
				}
				config["gitcache"] = map[string]any{"shadow": false}
			}
			release := map[string]any{"name": "oberth", "namespace": "oberth", "version": 1, "info": map[string]any{"status": "deployed"}, "chart": map[string]any{"metadata": map[string]any{"apiVersion": "v2", "name": "oberth", "version": "0.17.62"}, "values": map[string]any{"compatibility": map[string]any{"schemaIdentity": testUpgradeCompatibility.SchemaIdentity, "auditDomain": testUpgradeCompatibility.AuditDomain, "witnessKeyInfo": testUpgradeCompatibility.WitnessKeyInfo}}}, "config": config}
			raw, err := json.Marshal([]any{release})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(history, raw, 0600); err != nil {
				t.Fatal(err)
			}
			chart, err := filepath.Abs("../../charts/oberth")
			if err != nil {
				t.Fatal(err)
			}
			runHelm := upgradeTestHelm(t, dir, history)
			deps := Deps{RunHelm: runHelm}
			cfg := UpgradeConfig{Namespace: "oberth", ChartOverride: chart}
			if route == "values-file" {
				path := filepath.Join(dir, "values.yaml")
				if err = os.WriteFile(path, []byte(numericValues), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.UserValues = []string{path}
			}
			for pass := 0; pass < 2; pass++ {
				image := "example.invalid/oberth@sha256:" + strings.Repeat("1", 64)
				prepared, err := prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.cleanup()
				cfg.storedValues = prepared.storedValues
				if pass == 0 && route == "stored-migration" && cfg.storedValues == "" {
					t.Fatal("did not exercise checked migration snapshot")
				}
				args, flags := upgradeHelmArgs(cfg, chart, image, prepared.compatibility)
				args = append(args, flags...)
				args = append(args, "--dry-run=client", "--output=json")
				raw, err = runHelm(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if err = printHelmValueChanges(t.Context(), deps, cfg.Namespace, raw, prepared.revision); err != nil {
					t.Fatal(err)
				}
				var rendered struct {
					Manifest string `json:"manifest"`
				}
				if err = json.Unmarshal(raw, &rendered); err != nil {
					t.Fatal(err)
				}
				argsFound := upgradeServerArgs(t, rendered.Manifest)
				for _, flag := range []string{"trusted-tests-kvm-group", "argo-kubevirt-max-vms", "argo-kubevirt-max-cores"} {
					if _, ok := argsFound[flag]; ok {
						t.Errorf("retired flag restored: %s", flag)
					}
				}
				for flag, want := range expected {
					value, ok := argsFound[flag]
					got, err := strconv.ParseInt(value, 10, 64)
					if !ok || err != nil || got != want || value != strconv.FormatInt(want, 10) {
						t.Errorf("roundtrip%d --%s=%s: parsed %d (%v), want exact decimal %d", pass, flag, value, got, err, want)
					}
				}
				if argsFound["kubernetes-client-qps"] != "50.5" {
					t.Errorf("floating-point QPS was changed: %s", argsFound["kubernetes-client-qps"])
				}
				// Save exactly Helm's returned release as deployed, then use its stored
				// config on the next checked upgrade with no --values override.
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err = decoder.Decode(&release); err != nil {
					t.Fatal(err)
				}
				release["info"].(map[string]any)["status"] = "deployed"
				raw, err = json.Marshal([]any{release})
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(history, raw, 0600); err != nil {
					t.Fatal(err)
				}
				cfg.UserValues = nil
				cfg.storedValues = ""
			}
		})
	}
}

func upgradeServerArgs(t *testing.T, manifest string) map[string]string {
	t.Helper()
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		var object json.RawMessage
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(object, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Kind != "Deployment" || deployment.Name != "oberth" {
			continue
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "oberth" {
				continue
			}
			out := map[string]string{}
			for _, arg := range container.Args {
				key, value, ok := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
				if ok {
					out[key] = value
				}
			}
			return out
		}
	}
	t.Fatal("rendered Oberth Deployment absent")
	return nil
}

// Integer formatting must not turn fractional/negative input into accepted
// configuration, and an explicitly disabled floor must remain omitted.
func TestUpgradeIntegerFlagSchemaAndDisabledFloor(t *testing.T) {
	requireHelmForUpgradeTest(t)
	for _, value := range []string{"admission.diskFloorBytes=0", "admission.diskFloorBytes=-1", "admission.diskFloorBytes=0.5", "kubernetesClient.burst=1.5", "argo.workflowTTL=1.5"} {
		t.Run(value, func(t *testing.T) {
			args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@sha256:" + strings.Repeat("1", 64), "--set-json", value}
			out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			if value == "admission.diskFloorBytes=0" {
				if err != nil {
					t.Fatalf("zero floor: %v: %s", err, out)
				}
				if _, present := upgradeServerArgs(t, string(out))["admission-disk-floor-bytes"]; present {
					t.Fatal("disabled floor rendered")
				}
			} else if err == nil {
				t.Fatalf("invalid integer configuration accepted: %s", value)
			}
		})
	}
}
