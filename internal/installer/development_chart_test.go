package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDevelopmentChartReportsCandidateVersion(t *testing.T) {
	for _, override := range []string{"", "v0.22.1-candidate.2"} {
		t.Run(override, func(t *testing.T) {
			cfg := Config{ChartPath: "../../charts/oberth", BinaryVersion: "v0.22.1-candidate.1", ChartVersion: override}
			original, err := os.ReadFile(filepath.Join(cfg.ChartPath, "Chart.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := verifyInstallRelease(t.Context(), &cfg, Deps{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
			staged := args[3]
			if staged == cfg.ChartPath || staged != cfg.developmentChart {
				t.Fatalf("install bypassed staged chart: %v", args)
			}
			want := cfg.BinaryVersion
			if override != "" {
				want = override
			}
			out, err := DefaultRunHelm(t.Context(), []string{"show", "chart", staged})
			if err != nil {
				t.Fatal(err)
			}
			var meta struct {
				Version    string `json:"version"`
				AppVersion string `json:"appVersion"`
			}
			if err := yaml.Unmarshal(out, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Version != strings.TrimPrefix(want, "v") || meta.AppVersion != want {
				t.Fatalf("chart metadata = %+v", meta)
			}
			// Helm's NOTES use .Chart.AppVersion, not an overridden display value.
			notes, err := os.ReadFile(filepath.Join(staged, "templates", "NOTES.txt"))
			if err != nil || !strings.Contains(string(notes), ".Chart.AppVersion") {
				t.Fatal("chart notes lost version source", err)
			}
			again, err := verifyInstallRelease(t.Context(), &cfg, Deps{})
			if err != nil {
				t.Fatal(err)
			}
			again()
			if _, err := os.Stat(staged); err != nil {
				t.Fatal("nested reader removed staged chart", err)
			}
			kept, err := os.ReadFile(filepath.Join(cfg.ChartPath, "Chart.yaml"))
			if err != nil || string(kept) != string(original) {
				t.Fatal("checkout changed", err)
			}
			cleanup()
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatal("staged chart was retained", err)
			}
		})
	}
}

func TestDevelopmentChartLeavesArchivesAndUnversionedBuildsAlone(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "chart.tgz")
	if err := os.WriteFile(archive, []byte("archive fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{ChartPath: archive, BinaryVersion: "v0.22.1"},
		{ChartPath: "../../charts/oberth", BinaryVersion: "dev"},
	} {
		cleanup, err := verifyInstallRelease(t.Context(), &cfg, Deps{})
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if cfg.developmentChart != "" || oberthChart(cfg) != cfg.ChartPath {
			t.Fatal("non-source/candidate chart changed", cfg.developmentChart)
		}
	}
}

func TestDevelopmentChartAcceptsExplicitVersionFromUnversionedCLI(t *testing.T) {
	cfg := Config{ChartPath: "../../charts/oberth", BinaryVersion: "dev", ChartVersion: "v0.22.1-candidate.3"}
	cleanup, err := verifyInstallRelease(t.Context(), &cfg, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(filepath.Join(oberthChart(cfg), "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := yaml.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["appVersion"] != cfg.ChartVersion || metadata["version"] != "0.22.1-candidate.3" {
		t.Fatal("explicit candidate version lost", metadata)
	}
}
