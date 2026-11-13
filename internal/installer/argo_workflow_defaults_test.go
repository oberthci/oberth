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
