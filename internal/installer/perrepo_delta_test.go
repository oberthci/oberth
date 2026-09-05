package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- produced==0 with prior non-empty argo.perRepoIdentities → loud warning (#260 gap 6) ---

func TestWarnPerRepoIdentityDeltaPrintsWarning(t *testing.T) {
	t.Parallel()
	priorValues := map[string]any{
		"argo": map[string]any{
			"perRepoIdentities": []string{
				"oberth-argo-codeberg-oberthci-oberth-abc123",
				"oberth-argo-github-skipops-terraform-def456",
			},
		},
	}
	valuesJSON, _ := json.Marshal(priorValues)

	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			// Verify the helm args are correct.
			if len(args) < 6 {
				return nil, fmt.Errorf("unexpected helm args: %v", args)
			}
			if args[0] != "get" || args[1] != "values" || args[2] != "oberth" {
				return nil, fmt.Errorf("unexpected helm command: %v", args)
			}
			return valuesJSON, nil
		},
	}

	warnPerRepoIdentityDelta(context.Background(), deps, "oberth")

	text := output.String()
	if !strings.Contains(text, "per-repo identity produce returned 0 identities") {
		t.Fatalf("expected loud warning about 0 identities, got: %q", text)
	}
	if !strings.Contains(text, "2 (argo.perRepoIdentities)") {
		t.Fatalf("expected count of prior identities, got: %q", text)
	}
	if !strings.Contains(text, "--reuse-values") {
		t.Fatalf("expected mention of --reuse-values, got: %q", text)
	}
}

func TestWarnPerRepoIdentityDeltaHelmErrorSilent(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			// Simulate fresh install — no release.
			return nil, fmt.Errorf("release: not found")
		},
	}

	warnPerRepoIdentityDelta(context.Background(), deps, "oberth")

	if output.Len() > 0 {
		t.Fatalf("expected no output when helm get values fails, got: %q", output.String())
	}
}

func TestWarnPerRepoIdentityDeltaEmptyPriorSilent(t *testing.T) {
	t.Parallel()
	priorValues := map[string]any{
		"argo": map[string]any{
			"perRepoIdentities": []string{},
		},
	}
	valuesJSON, _ := json.Marshal(priorValues)

	var output bytes.Buffer
	deps := Deps{
		Output: &output,
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			return valuesJSON, nil
		},
	}

	warnPerRepoIdentityDelta(context.Background(), deps, "oberth")

	if output.Len() > 0 {
		t.Fatalf("expected no output when prior identities are empty, got: %q", output.String())
	}
}

func TestWarnPerRepoIdentityDeltaNilRunHelmSilent(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	deps := Deps{
		Output:  &output,
		RunHelm: nil,
	}

	warnPerRepoIdentityDelta(context.Background(), deps, "oberth")

	if output.Len() > 0 {
		t.Fatalf("expected no output when RunHelm is nil, got: %q", output.String())
	}
}
