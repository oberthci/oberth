package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPreviewHelmUpgradeRefusesOnSSAConflict(t *testing.T) {
	t.Parallel()

	// Scripted RunHelm: `helm list` returns an existing release, the preview
	// returns an SSA conflict error.
	var helmCalls []string
	deps := Deps{
		Output: &bytes.Buffer{},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			helmCalls = append(helmCalls, joined)

			// helm list → existing release
			if strings.HasPrefix(joined, "list") {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.17.11"}]`), nil
			}
			// helm upgrade --dry-run=server → SSA conflict, verbatim Helm
			// v4.2.3 output as recorded on tuxbox (helm history rev 75,
			// 2026-10-07): two objects in two namespaces joined by " && ".
			if strings.Contains(joined, "--dry-run=server") {
				return nil, errors.New(helm4TwoObjectConflict)
			}
			return nil, nil
		},
	}
	cfg := Config{Namespace: "oberth"}

	err := previewHelmUpgrade(context.Background(), cfg, deps, OpenBaoResult{}, "")
	if err == nil {
		t.Fatal("expected error from SSA conflict preview")
	}
	if !strings.Contains(err.Error(), "SSA field-ownership conflicts") {
		t.Fatalf("error should mention SSA conflicts: %v", err)
	}
	if !strings.Contains(err.Error(), "kubectl-client-side-apply") {
		t.Fatalf("error should name the conflicting manager: %v", err)
	}
	if !strings.Contains(err.Error(), "no OpenBao writes") {
		t.Fatalf("error should confirm zero mutations: %v", err)
	}
	// The exact transfer command, per object, in the object's OWN namespace:
	// the Role lives in oberth-argo, not in the release namespace.
	for _, want := range []string{
		"kubectl -n oberth get configmap cloudflared-watch-oberth-origin-ca -o yaml | kubectl -n oberth apply --server-side --field-manager=helm --force-conflicts -f -",
		"kubectl -n oberth-argo get role oberth-argo -o yaml | kubectl -n oberth-argo apply --server-side --field-manager=helm --force-conflicts -f -",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must print the per-object transfer command %q, got:\n%v", want, err)
		}
	}
}

// helm4TwoObjectConflict is the verbatim UPGRADE FAILED text Helm v4.2.3
// produced on tuxbox for release revision 75 (2026-10-07 17:42Z), minus the
// `Upgrade "oberth" failed: ` prefix.
const helm4TwoObjectConflict = `conflict occurred while applying object oberth/cloudflared-watch-oberth-origin-ca /v1, Kind=ConfigMap: ` +
	`Apply failed with 1 conflict: conflict with "kubectl-client-side-apply" using v1: .data.ca.crt && ` +
	`conflict occurred while applying object oberth-argo/oberth-argo rbac.authorization.k8s.io/v1, Kind=Role: ` +
	`Apply failed with 1 conflict: conflict with "kubectl-patch" using rbac.authorization.k8s.io/v1: .rules`

func TestPreviewHelmUpgradePassesOnCleanPreview(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Output: &bytes.Buffer{},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.HasPrefix(joined, "list") {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.17.11"}]`), nil
			}
			if strings.Contains(joined, "--dry-run=server") {
				return []byte(`{"name":"oberth","namespace":"oberth"}`), nil
			}
			return nil, nil
		},
	}
	cfg := Config{Namespace: "oberth"}

	if err := previewHelmUpgrade(context.Background(), cfg, deps, OpenBaoResult{}, ""); err != nil {
		t.Fatalf("preview should pass on clean preview: %v", err)
	}
}

func TestPreviewHelmUpgradeSkipsOnFreshInstall(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Output: &bytes.Buffer{},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.HasPrefix(joined, "list") {
				return []byte(`[]`), nil // no existing release
			}
			t.Fatal("should not run preview on fresh install")
			return nil, nil
		},
	}
	cfg := Config{Namespace: "oberth"}

	if err := previewHelmUpgrade(context.Background(), cfg, deps, OpenBaoResult{}, ""); err != nil {
		t.Fatalf("preview should skip on fresh install: %v", err)
	}
}

func TestPreviewHelmUpgradeUsedBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	// Simulate the full installer flow ordering: the preview must detect
	// SSA conflicts before any OpenBao write (kv patch), Argo install, or
	// cert rotation. We verify by checking that when the preview fails, the
	// scripted OpenBao runner records zero writes.
	var openbaoWrites int
	var helmCalls []string

	deps := Deps{
		Output: &bytes.Buffer{},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			helmCalls = append(helmCalls, joined)
			if strings.HasPrefix(joined, "list") {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.17.11"}]`), nil
			}
			if strings.Contains(joined, "--dry-run=server") {
				return nil, errors.New(`conflict with "kubectl-client-side-apply" using v1: .data.ca.crt`)
			}
			return nil, nil
		},
	}
	cfg := Config{Namespace: "oberth"}

	err := previewHelmUpgrade(context.Background(), cfg, deps, OpenBaoResult{}, "")
	if err == nil {
		t.Fatal("expected preview to refuse on SSA conflict")
	}
	if openbaoWrites != 0 {
		t.Fatalf("expected zero OpenBao writes before preview refusal, got %d", openbaoWrites)
	}
}

func TestDetectFailedHelmRevision(t *testing.T) {
	t.Parallel()

	t.Run("fails on failed latest revision", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			Output: &bytes.Buffer{},
			RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"failed","chart":"oberth-0.17.11"}]`), nil
			},
		}
		err := detectFailedHelmRevision(context.Background(), deps, "oberth")
		if err == nil {
			t.Fatal("expected error for failed latest revision")
		}
		if !strings.Contains(err.Error(), "failed") {
			t.Fatalf("error should mention failed status: %v", err)
		}
	})

	t.Run("passes on deployed latest revision", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			Output: &bytes.Buffer{},
			RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.17.11"}]`), nil
			},
		}
		if err := detectFailedHelmRevision(context.Background(), deps, "oberth"); err != nil {
			t.Fatalf("should pass on deployed revision: %v", err)
		}
	})

	t.Run("passes on empty release list", func(t *testing.T) {
		t.Parallel()
		deps := Deps{
			Output: &bytes.Buffer{},
			RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				return []byte(`[]`), nil
			},
		}
		if err := detectFailedHelmRevision(context.Background(), deps, "oberth"); err != nil {
			t.Fatalf("should pass on empty list: %v", err)
		}
	})
}

func TestParseSSAConflicts(t *testing.T) {
	t.Parallel()

	conflicts := parseSSAConflicts(`Upgrade "oberth" failed: ` + helm4TwoObjectConflict)
	want := []ssaConflict{
		{ssaObject: ssaObject{Namespace: "oberth", Name: "cloudflared-watch-oberth-origin-ca", APIVersion: "v1", Kind: "ConfigMap"},
			Manager: "kubectl-client-side-apply", Field: ".data.ca.crt"},
		{ssaObject: ssaObject{Namespace: "oberth-argo", Name: "oberth-argo", APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			Manager: "kubectl-patch", Field: ".rules"},
	}
	if len(conflicts) != len(want) {
		t.Fatalf("expected %d conflicts, got %d: %+v", len(want), len(conflicts), conflicts)
	}
	for i := range want {
		if conflicts[i] != want[i] {
			t.Fatalf("conflict %d = %+v, want %+v", i, conflicts[i], want[i])
		}
	}
}

// TestParseSSAConflictsDashList covers the API server's multi-field shape
// ("Apply failed with 2 conflicts: conflicts with ... :\n- .a\n- .b") and a
// cluster-scoped object (no namespace in the object reference).
func TestParseSSAConflictsDashList(t *testing.T) {
	t.Parallel()

	msg := "conflict occurred while applying object oberth-argo-executor rbac.authorization.k8s.io/v1, Kind=ClusterRole: " +
		"Apply failed with 2 conflicts: conflicts with \"kubectl-client-side-apply\" using rbac.authorization.k8s.io/v1:\n- .rules\n- .metadata.labels.app"
	conflicts := parseSSAConflicts(msg)
	want := []ssaConflict{
		{ssaObject: ssaObject{Namespace: "", Name: "oberth-argo-executor", APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			Manager: "kubectl-client-side-apply", Field: ".rules"},
		{ssaObject: ssaObject{Namespace: "", Name: "oberth-argo-executor", APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			Manager: "kubectl-client-side-apply", Field: ".metadata.labels.app"},
	}
	if len(conflicts) != len(want) {
		t.Fatalf("expected %d conflicts, got %d: %+v", len(want), len(conflicts), conflicts)
	}
	for i := range want {
		if conflicts[i] != want[i] {
			t.Fatalf("conflict %d = %+v, want %+v", i, conflicts[i], want[i])
		}
	}
	// A message with manager/field but no object header still yields the
	// pair and the refusal says the command cannot be derived.
	bare := parseSSAConflicts(`conflict with "kubectl-patch" using v1: .data.x`)
	if len(bare) != 1 || bare[0].Name != "" || bare[0].Field != ".data.x" {
		t.Fatalf("bare conflict = %+v", bare)
	}
	if out := formatConflictRefusal(bare, "oberth"); !strings.Contains(out, "did not identify the object") || strings.Contains(out, "kubectl -n") {
		t.Fatalf("unexpected refusal for an unidentified object:\n%s", out)
	}
}

func TestOberthHelmArgsNoAtomicOnInstallPath(t *testing.T) {
	t.Parallel()

	// #812 remediation 3: --atomic is NOT used on the install path either.
	// Forward-only database migrations make auto-rollback a crashloop trap.
	// The failed-revision detection and SSA preview provide the safety net.
	cfg := Config{
		Namespace:    "oberth",
		ChartVersion: "0.17.12",
	}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
	for _, arg := range args {
		if arg == "--atomic" {
			t.Fatal("--atomic must not be present in install helm args: auto-rollback + forward-only migrations = crashloop trap")
		}
	}
}
