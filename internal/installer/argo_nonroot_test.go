package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
					return json.Marshal(nonrootArgoValues(DefaultArgoNamespace))
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
		_, cleanup, err := prepareNonrootArgoValues(t.Context(), Config{ArgoControllerProfile: argoworkflow.NonrootStaticProfile, ArgoNamespace: DefaultArgoNamespace}, deps, true, DefaultArgoChartVersion)
		cleanup()
		if err == nil {
			t.Fatal("invalid existing profile accepted")
		}
	}
}
