package installer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func withInstallDryRunRelease(t testing.TB, deps *Deps, cfg *Config) {
	t.Helper()
	cfg.ChartVersion = "0.22.7"
	withSignedRelease(t, deps, cfg.ChartVersion, fakeChartImageRef)
	original := deps.RunHelm
	deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 1 && args[0] == "show" && args[1] == "values" {
			return []byte("image:\n  ref: " + fakeChartImageRef + "\n"), nil
		}
		if args[0] == "get" && args[1] == "values" {
			return []byte(`{}`), nil
		}
		if args[0] == "upgrade" && slices.Contains(args, "--dry-run=server") && slices.Contains(args, "--no-hooks") {
			return []byte(`{"config":{}}`), nil
		}
		return original(ctx, args)
	}
}

func TestInstallDryRunRendersFreshAndExistingCharts(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing"}[existing], func(t *testing.T) {
			previews := 0
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					if existing {
						return []byte(`[{"name":"oberth","chart":"oberth-0.22.7","status":"deployed"}]`), nil
					}
					return []byte(`[]`), nil
				case "get":
					return []byte(`{}`), nil
				case "upgrade":
					if !slices.Contains(args, "--dry-run=server") || !slices.Contains(args, "--no-hooks") {
						t.Fatalf("install dry run attempted a write: %v", args)
					}
					previews++
					return nil, errors.New("additional properties 'operatorUnknown' not allowed")
				default:
					t.Fatalf("unexpected Helm command: %v", args)
					return nil, nil
				}
			}}
			err := previewHelmUpgrade(t.Context(), Config{Namespace: "oberth", ChartPath: "target-chart", DryRun: true}, deps, OpenBaoResult{})
			if err == nil || !strings.Contains(err.Error(), "additional properties 'operatorUnknown' not allowed") || previews != 1 {
				t.Fatalf("install dry run missed render validation: previews=%d err=%v", previews, err)
			}
		})
	}
}

func TestRestorePreviewSchemaRefusalPrecedesWrites(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply", true: "dry-run"}[dryRun], func(t *testing.T) {
			archive, runID, _ := backupFixtureArchive(t, false)
			target := restoreTargetCluster(t, runID)
			var output bytes.Buffer
			deps := target.deps(&output)
			original := deps.RunHelm
			previews := 0
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "upgrade" && slices.Contains(args, "--dry-run=server") {
					previews++
					return nil, errors.New("additional properties 'operatorUnknown' not allowed")
				}
				return original(ctx, args)
			}
			cfg := RestoreConfig{Input: archive, Yes: true, DryRun: dryRun, Timeout: time.Second}
			cfg.applyDefaults()
			err := runRestore(t.Context(), cfg, deps)
			if err == nil || !strings.Contains(err.Error(), "additional properties 'operatorUnknown' not allowed") || previews != 1 {
				t.Fatalf("restore missed preview: previews=%d err=%v", previews, err)
			}
			assertNoRestoreMutation(t, target)
		})
	}
}

func TestUpgradeDryRunRendersChangedDefaults(t *testing.T) {
	fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
	original := fixture.deps.RunHelm
	templatePinned, previewPinned := false, false
	fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 1 && args[0] == "show" && args[1] == "values" {
			return []byte("image:\n  ref: " + fakeChartImageRef + "\n" + v021Defaults), nil
		}
		if args[0] == "template" {
			templatePinned = slices.Contains(args, "retention.logs.budgetBytes=0")
		}
		if args[0] == "upgrade" {
			if !slices.Contains(args, "--dry-run=server") {
				t.Fatalf("dry run attempted a write: %v", args)
			}
			previewPinned = slices.Contains(args, "retention.logs.budgetBytes=0")
		}
		return original(ctx, args)
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true, DryRun: true}, fixture.deps)
	if err != nil || !templatePinned || !previewPinned || !strings.Contains(fixture.output.String(), "Keeping retention.logs.budgetBytes=0") {
		t.Fatalf("dry run omitted default pins: template=%v preview=%v err=%v\n%s", templatePinned, previewPinned, err, fixture.output.String())
	}
}
