package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestNonrootArgoInstallerProfile(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			var calls [][]string
			deps := argoTestDeps(t, &calls)
			var profilePath string
			deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
				calls = append(calls, slices.Clone(args))
				switch args[0] {
				case "list":
					if existing {
						return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
					}
					return []byte("[]"), nil
				case "get":
					return json.Marshal(nonrootArgoValues(DefaultArgoNamespace, baseNonrootProfile()))
				case "upgrade":
					index := slices.Index(args, "--values")
					if index < 0 || index+1 >= len(args) {
						t.Fatal("profile not supplied to normal Helm operation")
					}
					profilePath = args[index+1]
					stat, err := os.Stat(profilePath)
					if err != nil || stat.Mode().Perm() != 0o600 {
						t.Fatalf("profile file permissions: %v", err)
					}
					data, err := os.ReadFile(profilePath)
					if err != nil {
						t.Fatal(err)
					}
					var values map[string]any
					if err := json.Unmarshal(data, &values); err != nil {
						t.Fatal(err)
					}
					if err := validateNonrootReuse(values, DefaultArgoNamespace); err != nil {
						t.Fatal(err)
					}
					if directory := os.Getenv("OBERTH_NONROOT_HELM_DIR"); directory != "" {
						if err := os.WriteFile(filepath.Join(directory, "profile-values.json"), data, 0o600); err != nil {
							t.Fatal(err)
						}
						encoded, err := json.Marshal(ArgoHelmArgs(Config{}))
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(directory, "installer-helm-args.json"), encoded, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if fail {
						return nil, errors.New("controlled Helm failure")
					}
				}
				return nil, nil
			}
			result, err := InstallArgoWorkflows(t.Context(), Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile}, deps)
			if (err != nil) != fail {
				t.Fatalf("installer failure=%v error=%v", fail, err)
			}
			if existing && !result.Upgraded {
				t.Fatal("profile was silently skipped on existing installation")
			}
			if profilePath == "" {
				t.Fatal("profile was not supplied")
			}
			if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
				t.Fatal("temporary profile remains after operation")
			}
		}
	}
}

func TestNonrootArgoRejectsCustomReuse(t *testing.T) {
	cases := []string{
		`{"controller":{"config":{"artifactRepository":{"archiveLogs":true}}}}`,
		`{"controller":{"extraArgs":["--executor-plugins"]}}`,
		`{"controller":{"image":{"repository":"other"}}}`,
		`{"controller":{"volumeMounts":[{"name":"other","mountPath":"/bin"}]}}`,
		`{"controller":{"extraEnv":[{"name":"LD_PRELOAD","value":"private"}]}}`,
		`{"executor":{"env":[{"name":"INJECT","value":"private"}]}}`,
		`{"extraObjects":[{"kind":"Secret"}]}`,
		`{"singleNamespace":false}`,
		`{"controller":{"priorityClassName":"oberth-platform","resources":{"limits":{"memory":"128Mi"}}}}`,
		`{"controller":{"resources":{"requests":{"cpu":"25m","memory":"128Mi","ephemeral-storage":"1Gi"}}}}`,
		`{"controller":{"resources":{"requests":{"cpu":"25m"},"claims":[]}}}`,
		`{"controller":{"resources":{"requests":{"cpu":"0"}}}}`,
		`{"controller":{"resources":{"requests":{"memory":"-1Gi"}}}}`,
		`{"controller":{"resources":{"requests":{"memory":"invalid"}}}}`,
		`{"controller":{"resources":{"requests":{"cpu":25}}}}`,
		`{"controller":{"resources":{"requests":null}}}`,
		`{"controller":{"resources":{"requests":{"cpu":{"value":"25m"}}}}}`,
		`{"controller":{"resources":"25m"}}`,
		`{"controller":{"priorityClassName":true}}`,
		`{"controller":{"priorityClassName":"invalid/name"}}`,
		`{"executor":{"resources":{"requests":{"cpu":"25m"}}}}`,
	}
	for _, data := range cases {
		var values map[string]any
		if err := json.Unmarshal([]byte(data), &values); err != nil {
			t.Fatal(err)
		}
		if err := validateNonrootReuse(values, DefaultArgoNamespace); err == nil {
			t.Fatal("custom controller settings accepted")
		}
	}
	for _, cfg := range []Config{
		{ArgoControllerProfile: "unknown"},
		{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, SkipArgo: true},
		{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoChartVersion: "1.0.25"},
	} {
		var calls [][]string
		if _, err := InstallArgoWorkflows(t.Context(), cfg, argoTestDeps(t, &calls)); err == nil || len(calls) != 0 {
			t.Fatal("unsupported profile caused a Helm operation")
		}
	}
	for _, response := range []string{`{"controller":{"config":{"archiveLogs":true}}}`, "not-json"} {
		var calls [][]string
		deps := argoTestDeps(t, &calls)
		deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
			if args[0] != "get" {
				t.Fatal("unsupported settings caused mutation")
			}
			return []byte(response), nil
		}
		_, _, cleanup, err := prepareNonrootArgoValues(t.Context(), Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace}, deps, true, DefaultArgoChartVersion)
		cleanup()
		if err == nil {
			t.Fatal("invalid existing profile accepted")
		}
	}
}

func TestNonrootArgoAcceptsPlatformProtectionReuse(t *testing.T) {
	body, err := os.ReadFile("../../deploy/platform-protection/argo-workflows-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var overlay map[string]any
	if err := yaml.Unmarshal(body, &overlay); err != nil {
		t.Fatal(err)
	}
	if _, ok := overlay["controller"].(map[string]any)["resources"]; ok {
		t.Fatal("priority overlay must not impose our installation's resource sizing")
	}
	for _, profile := range []bool{false, true} {
		values := map[string]any{}
		if profile {
			values = nonrootArgoValues(DefaultArgoNamespace, baseNonrootProfile())
		}
		values["singleNamespace"] = true
		values["server"] = map[string]any{"enabled": false}
		controller, ok := values["controller"].(map[string]any)
		if !ok {
			controller = map[string]any{}
			values["controller"] = controller
		}
		controller["clusterWorkflowTemplates"] = map[string]any{"enabled": false}
		controller["extraEnv"] = []any{map[string]any{"name": podNamesEnv, "value": podNamesFormat}}
		for key, value := range overlay["controller"].(map[string]any) {
			controller[key] = value
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] != "get" {
				t.Fatal("reuse validation caused mutation")
			}
			return encoded, nil
		}}
		file, _, cleanup, err := prepareNonrootArgoValues(t.Context(), Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace}, deps, true, DefaultArgoChartVersion)
		if err != nil {
			cleanup()
			t.Fatalf("profile=%v: platform overlay refused: %v", profile, err)
		}
		actual, readErr := os.ReadFile(file)
		cleanup()
		if readErr != nil {
			t.Fatal(readErr)
		}
		want, err := json.Marshal(nonrootArgoValues(DefaultArgoNamespace, baseNonrootProfile()))
		if err != nil || string(actual) != string(want) {
			t.Fatal("profile overlay changed or overwrote reused platform settings")
		}
	}
	for _, body := range []string{
		`{"controller":{"priorityClassName":"other-namespace-platform"}}`,
		`{"controller":{"priorityClassName":""}}`,
		`{"controller":{"resources":{"requests":{"cpu":"1","memory":"256Mi"}}}}`,
	} {
		var values map[string]any
		if err := json.Unmarshal([]byte(body), &values); err != nil {
			t.Fatal(err)
		}
		if err := validateNonrootReuse(values, DefaultArgoNamespace); err != nil {
			t.Fatal(err)
		}
	}
}

// The run limits the installer writes into the argo-workflows release (#1517)
// are not profile customization: the controller's parallelism and the limits
// ConfigMap beside the profile object are accepted on reuse, a foreign object
// or a non-integer parallelism is still refused, a profile re-run without
// --argo-sync-limit carries the stored ConfigMap into its values file, and
// a run with limits leaves it to --set-json.
func TestNonrootArgoReuseAcceptsInstallerLimits(t *testing.T) {
	limits := syncLimitsConfigMap(map[string]int{"kvm": 1, "heavy-go": 2}, DefaultArgoNamespace)
	roundTrip := func(t *testing.T, values map[string]any) (map[string]any, []byte) {
		t.Helper()
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		var stored map[string]any
		if err := json.Unmarshal(encoded, &stored); err != nil {
			t.Fatal(err)
		}
		return stored, encoded
	}
	withLimits := nonrootArgoValues(DefaultArgoNamespace, baseNonrootProfile())
	withLimits["controller"].(map[string]any)["parallelism"] = 4
	withLimits["controller"].(map[string]any)["namespaceParallelism"] = 0
	withLimits["extraObjects"] = append(withLimits["extraObjects"].([]any), limits)
	stored, encoded := roundTrip(t, withLimits)
	if err := validateNonrootReuse(stored, DefaultArgoNamespace); err != nil {
		t.Fatalf("installer limits refused on reuse: %v", err)
	}
	// Limits set before the profile: extraObjects holds the ConfigMap alone.
	before, _ := roundTrip(t, map[string]any{"extraObjects": []any{limits}, "controller": map[string]any{"parallelism": 2}})
	if err := validateNonrootReuse(before, DefaultArgoNamespace); err != nil {
		t.Fatalf("limits set before the profile refused: %v", err)
	}
	for _, data := range []string{
		`{"controller":{"parallelism":-1}}`,
		`{"controller":{"parallelism":1.5}}`,
		`{"controller":{"namespaceParallelism":"4"}}`,
		`{"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"oberth-sync-limits","namespace":"oberth-argo"},"data":{"kvm":"1"}},{"kind":"Secret"}]}`,
		`{"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"oberth-sync-limits","namespace":"elsewhere"},"data":{"kvm":"1"}}]}`,
		`{"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"oberth-sync-limits","namespace":"oberth-argo"},"data":{"kvm":"x"}}]}`,
	} {
		var values map[string]any
		if err := json.Unmarshal([]byte(data), &values); err != nil {
			t.Fatal(err)
		}
		if err := validateNonrootReuse(values, DefaultArgoNamespace); err == nil {
			t.Fatalf("accepted on reuse: %s", data)
		}
	}
	profileValues := func(t *testing.T, cfg Config) []any {
		t.Helper()
		deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] != "get" {
				t.Fatal("reuse validation caused mutation")
			}
			return encoded, nil
		}}
		file, _, cleanup, err := prepareNonrootArgoValues(t.Context(), cfg, deps, true, DefaultArgoChartVersion)
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		body, readErr := os.ReadFile(file)
		cleanup()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var values map[string]any
		if err := json.Unmarshal(body, &values); err != nil {
			t.Fatal(err)
		}
		return values["extraObjects"].([]any)
	}
	cfg := Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace}
	objects := profileValues(t, cfg)
	if len(objects) != 2 || !isSyncLimitsConfigMap(objects[1], DefaultArgoNamespace) {
		t.Fatalf("profile re-run dropped the stored limits ConfigMap: %v", objects)
	}
	cfg.ArgoSyncLimits = map[string]int{"kvm": 3}
	if objects := profileValues(t, cfg); len(objects) != 1 {
		t.Fatalf("requested limits duplicated in the profile values: %v", objects)
	}
}

// Stored values outside the allowlist refuse the profile unless the operator
// acknowledges exactly the bytes reviewed; placement keys need no review.
func TestNonrootReuseAcknowledgesReviewedValues(t *testing.T) {
	t.Parallel()
	placement := `{"controller":{"nodeSelector":{"kubernetes.io/os":"linux"},"tolerations":[{"key":"ci","operator":"Exists"}]}}`
	var values map[string]any
	if err := json.Unmarshal([]byte(placement), &values); err != nil {
		t.Fatal(err)
	}
	if err := checkNonrootReuse([]byte(placement), values, DefaultArgoNamespace, ""); err != nil {
		t.Fatalf("placement-only values refused: %v", err)
	}
	for _, bad := range []string{`{"controller":{"nodeSelector":{"a":1}}}`, `{"controller":{"tolerations":"all"}}`} {
		var values map[string]any
		if err := json.Unmarshal([]byte(bad), &values); err != nil {
			t.Fatal(err)
		}
		if err := validateNonrootReuse(values, DefaultArgoNamespace); err == nil {
			t.Fatalf("malformed placement %s accepted", bad)
		}
	}

	custom := []byte(`{"controller":{"podAnnotations":{"team":"ci"}}}` + "\n")
	if err := json.Unmarshal(custom, &values); err != nil {
		t.Fatal(err)
	}
	err := checkNonrootReuse(custom, values, DefaultArgoNamespace, "")
	if err == nil || !strings.Contains(err.Error(), "--argo-values-reviewed") || !strings.Contains(err.Error(), "helm get values argo-workflows -n "+DefaultArgoNamespace+" -o json") {
		t.Fatalf("unreviewed custom values: %v", err)
	}
	sum := sha256.Sum256(custom)
	digest := hex.EncodeToString(sum[:])
	for _, reviewed := range []string{"sha256:" + digest, digest} {
		if err := checkNonrootReuse(custom, values, DefaultArgoNamespace, reviewed); err != nil {
			t.Fatalf("reviewed %q refused: %v", reviewed, err)
		}
	}
	changed := []byte(`{"controller":{"podAnnotations":{"team":"other"}}}` + "\n")
	if err := checkNonrootReuse(changed, values, DefaultArgoNamespace, "sha256:"+digest); err == nil {
		t.Fatal("values changed after the review accepted")
	}

	// The install path: the reviewed digest is of what Helm returned.
	deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] != "get" {
			t.Fatalf("unexpected Helm call %v", args)
		}
		return custom, nil
	}}
	cfg := Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace}
	_, _, cleanup, err := prepareNonrootArgoValues(t.Context(), cfg, deps, true, DefaultArgoChartVersion)
	cleanup()
	if err == nil {
		t.Fatal("unreviewed custom values accepted on install")
	}
	cfg.ArgoValuesReviewed = "sha256:" + digest
	_, _, cleanup, err = prepareNonrootArgoValues(t.Context(), cfg, deps, true, DefaultArgoChartVersion)
	cleanup()
	if err != nil {
		t.Fatalf("reviewed custom values refused on install: %v", err)
	}
}
