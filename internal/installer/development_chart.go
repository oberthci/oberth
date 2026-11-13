package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// A source checkout's Chart.yaml is stamped by release packaging, so it can lag
// behind a candidate binary. Stamp a private copy for this development install;
// published archives and signed release verification retain their own metadata.
func stageDevelopmentChart(cfg *Config) (func(), error) {
	noop := func() {}
	if cfg.developmentChart != "" || (canonicalChartVersion(cfg.BinaryVersion) == "" && canonicalChartVersion(cfg.ChartVersion) == "") {
		return noop, nil
	}
	source := strings.TrimSpace(cfg.ChartPath)
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("read development chart: %w", err)
	}
	if !info.IsDir() {
		return noop, nil
	}
	version := canonicalChartVersion(cfg.ChartVersion)
	if version == "" {
		version = canonicalChartVersion(cfg.BinaryVersion)
	}
	raw, err := os.ReadFile(filepath.Join(source, "Chart.yaml")) //nolint:gosec // G304: metadata from the explicitly selected development chart.
	if err != nil {
		return nil, fmt.Errorf("read development chart metadata: %w", err)
	}
	metadata := map[string]any{}
	if err := yaml.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode development chart metadata: %w", err)
	}
	metadata["version"], metadata["appVersion"] = strings.TrimPrefix(version, "v"), version
	raw, err = yaml.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode development chart metadata: %w", err)
	}
	dir, err := os.MkdirTemp("", "oberth-development-chart-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
		cleanup()
		return nil, fmt.Errorf("copy development chart: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), raw, 0o600); err != nil {
		cleanup()
		return nil, err
	}
	cfg.developmentChart = dir
	return cleanup, nil
}
