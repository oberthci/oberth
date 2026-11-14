package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestArgoWorkflowTTLDefaultsAreAbsentOnlyAndReviewed(t *testing.T) {
	for _, stored := range []string{`{}`, `{"controller":{"workflowDefaults":{"metadata":{"labels":{"operator":"kept"}}}}}`, `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":null}}}}`, `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{}}}}}`, `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":0,"secondsAfterSuccess":123,"secondsAfterFailure":456}}}}}`, `{"controller":{"workflowDefaults":null}}`} {
		t.Run(stored, func(t *testing.T) {
			calls := 0
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				calls++
				if slices.Contains(args, "--all") || args[0] != "get" {
					t.Fatal(args)
				}
				return []byte(stored), nil
			}}
			review, err := readArgoWorkflowTTL(t.Context(), deps, "pipeline", true)
			if err != nil {
				t.Fatal(err)
			}
			explicit := strings.Contains(stored, "ttlStrategy") || strings.Contains(stored, "workflowDefaults\":null")
			cfg := Config{argoWorkflowTTL: review}
			flags := strings.Join(ArgoHelmArgs(cfg), " ")
			if strings.Contains(flags, "controller.workflowDefaults.spec.ttlStrategy=") == explicit {
				t.Fatal("explicit TTL overwritten or default missing", flags)
			}
			view := Review(cfg).ArgoWorkflowTTL
			if view == nil || view.OperatorSet != explicit || calls != 1 {
				t.Fatal(view, calls)
			}
			var output bytes.Buffer
			printArgoWorkflowTTL(&output, view)
			if !strings.Contains(output.String(), "controller.workflowDefaults.spec.ttlStrategy") {
				t.Fatal(output.String())
			}
			if !explicit && !strings.Contains(output.String(), `"secondsAfterCompletion":3600`) {
				t.Fatal(output.String())
			}
		})
	}
	cfg := Config{SkipArgo: true}
	if Review(cfg).ArgoWorkflowTTL != nil || len(argoWorkflowTTLArgs(cfg)) != 0 {
		t.Fatal("skip Argo added a default")
	}
}

func TestArgoWorkflowTTLApplyRereadsOperatorValues(t *testing.T) {
	stored := `{}`
	client := fake.NewClientset(argoSecurityLiveObjects(DefaultArgoNamespace, true)...)
	var applied []string
	fail := false
	deps := Deps{KubeClient: client, Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			if fail {
				return nil, errors.New("read unavailable")
			}
			return []byte(stored), nil
		case "upgrade":
			applied = slices.Clone(args)
		}
		return nil, nil
	}}
	cfg := Config{ArgoSecurityDefaults: true}
	if err := resolveArgoExecutorResources(t.Context(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if Review(cfg).ArgoWorkflowTTL.OperatorSet {
		t.Fatal("initially absent")
	}
	stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":0}}}}}`
	if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(applied, " "), "controller.workflowDefaults.spec.ttlStrategy=") {
		t.Fatal("stale review overwrote operator TTL", applied)
	}
	fail = true
	applied = nil
	if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err == nil || len(applied) > 0 {
		t.Fatal("unreadable values allowed a write", applied, err)
	}
}

func TestNativeUpgradeRetainsDefaultWorkflowCleanup(t *testing.T) {
	for _, mode := range []string{"absent", "null", "empty", "zero", "custom", "ancestor-null", "concurrent-override", "unreadable", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
			stored := `{}`
			switch mode {
			case "null":
				stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":null}}}}`
			case "empty":
				stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{}}}}}`
			case "zero":
				stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":0}}}}}`
			case "custom":
				stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterSuccess":42}}}}}`
			case "ancestor-null":
				stored = `{"controller":{"workflowDefaults":null}}`
			}
			original := fixture.deps.RunHelm
			reads, writes := 0, 0
			fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "repo" {
					return nil, nil
				}
				if len(args) > 2 && args[0] == "get" && args[1] == "values" && args[2] == "argo-workflows" {
					reads++
					if mode == "unreadable" {
						return nil, errors.New("values unavailable")
					}
					if mode == "concurrent-override" && reads >= 3 {
						stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":0}}}}}`
					}
					return []byte(stored), nil
				}
				if len(args) > 1 && args[0] == "upgrade" && args[1] == "argo-workflows" {
					writes++
					if !slices.Contains(args, `controller.workflowDefaults.spec.ttlStrategy={"secondsAfterCompletion":3600}`) || !slices.Contains(args, "--reuse-values") {
						t.Fatal(args)
					}
					stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":3600}}}}}`
					return nil, nil
				}
				return original(ctx, args)
			}
			result, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true, DryRun: mode == "dry-run"}, fixture.deps)
			if mode == "unreadable" {
				if err == nil || writes != 0 {
					t.Fatal(result, writes, err)
				}
				return
			}
			if err != nil {
				t.Fatal(result, err)
			}
			expected := 0
			if mode == "absent" {
				expected = 1
			}
			if writes != expected {
				t.Fatal(mode, writes, expected, reads)
			}
			if !strings.Contains(fixture.output.String(), "controller.workflowDefaults.spec.ttlStrategy") {
				t.Fatal(fixture.output.String())
			}
		})
	}
}
