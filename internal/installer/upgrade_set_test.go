package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestUpgradeOverrideGrammarAndPins(t *testing.T) {
	for _, value := range []string{
		`replicas=1,image.ref=foreign`, `replicas=1,argo.controllerProfile=foreign`,
		`image[0]=foreign`, `image\.ref=foreign`, `image.ref[0]=foreign`,
		`image..ref=foreign`, `image\\.ref=foreign`, `replicas={1}`, `replicas={1,2}`,
		`replicas=1\\,image.ref=foreign`, `replicas=trailing\`, ` image.ref=foreign`,
	} {
		if _, err := upgradeSetKey(value); err == nil {
			t.Fatalf("unsupported Helm grammar accepted: %q", value)
		}
		if err := checkPinnedKeyConflicts([]string{value}, nil); err == nil {
			t.Fatalf("pin check accepted: %q", value)
		}
		if err := validateUserSetAgainstValues([]byte("replicas: 1\n"), []string{value}); err == nil {
			t.Fatalf("schema accepted: %q", value)
		}
	}
	for _, value := range []string{`replicas=1`, `replicas=literal\,comma`, `replicas=literal\,image.ref=not-an-assignment`, `replicas=literal\\\,comma`, `replicas=`, `replicas=two=equals`} {
		if key, err := upgradeSetKey(value); err != nil || key != "replicas" {
			t.Fatalf("scalar refused: %q: %v", value, err)
		}
		if err := checkPinnedKeyConflicts([]string{value}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{`image=foreign`, `image.ref=foreign`} {
		if err := checkPinnedKeyConflicts([]string{value}, nil); err == nil {
			t.Fatal("parent/exact image override accepted")
		}
	}
	for _, value := range []string{`argo=foreign`, `argo.namespace=foreign`, `argo.controllerProfile=foreign`, `argo.controllerProfile.child=foreign`} {
		if err := checkControllerProfileOverrides(UpgradeConfig{UserSet: []string{value}}); err == nil {
			t.Fatal("parent/exact/descendant profile override accepted")
		}
	}
}

// This uses the same Helm binary installed by native CI, not a model of strvals.
func TestUpgradeOverrideActualHelmSemantics(t *testing.T) {
	requireHelmForUpgradeTest(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Chart.yaml":           "apiVersion: v2\nname: public-override-proof\nversion: 0.0.1\n",
		"values.yaml":          "replicas: default\nimage:\n  ref: pinned\nargo:\n  controllerProfile: nonroot-static-v1\n",
		"templates/proof.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: public-proof\ndata:\n  image: {{ .Values.image.ref | quote }}\n  scalar: {{ .Values.replicas | quote }}\n  profile: {{ .Values.argo.controllerProfile | quote }}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		input, scalar, image, profile string
		allow                         bool
	}{
		{`replicas=literal\,comma`, "literal,comma", "pinned", "nonroot-static-v1", true},
		{`replicas=literal\,image.ref=not-an-assignment`, "literal,image.ref=not-an-assignment", "pinned", "nonroot-static-v1", true},
		{`replicas=1,image.ref=foreign,argo.controllerProfile=foreign`, "1", "foreign", "foreign", false},
	} {
		out, err := exec.CommandContext(t.Context(), "helm", "template", "proof", dir, "--set-string", tc.input).CombinedOutput()
		if err != nil {
			t.Fatalf("actual Helm fixture failed: %v", err)
		}
		var rendered struct {
			Data map[string]string `json:"data"`
		}
		if err := yaml.Unmarshal(out, &rendered); err != nil {
			t.Fatal(err)
		}
		if rendered.Data["image"] != tc.image || rendered.Data["profile"] != tc.profile || rendered.Data["scalar"] != tc.scalar {
			t.Fatal("Helm actual assignment semantics changed")
		}
		_, err = upgradeSetKey(tc.input)
		if (err == nil) != tc.allow {
			t.Fatal("upgrade acceptance differs from supported scalar grammar")
		}
	}
}
