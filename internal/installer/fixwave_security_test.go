package installer

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func fixwaveSecurityDeps(t *testing.T, safe bool, stored string) Deps {
	t.Helper()
	client := argoSecurityTestClient()
	setArgoSecurityLive(t, client, DefaultArgoNamespace, safe)
	return Deps{KubeClient: client, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(stored), nil
		default:
			t.Fatalf("security review attempted a write: %v", args)
			return nil, nil
		}
	}}
}

func TestArgoSecurityStoredUnsafeContextWithCompliantLiveDefaults(t *testing.T) {
	for _, component := range []string{"controller", "server", "mainContainer", "executor"} {
		t.Run(component, func(t *testing.T) {
			stored := fmt.Sprintf(`{%q:{"securityContext":{"allowPrivilegeEscalation":true}}}`, component)
			deps := fixwaveSecurityDeps(t, true, stored)
			if _, err := prepareArgoSecurity(t.Context(), deps, DefaultArgoNamespace, true, false, nil); err == nil || !strings.Contains(err.Error(), "operator-set "+component+".securityContext") {
				t.Fatalf("unsafe restart settings admitted: %v", err)
			}
		})
	}
	deps := fixwaveSecurityDeps(t, true, `{}`)
	plan, err := prepareArgoSecurity(t.Context(), deps, DefaultArgoNamespace, false, false, nil)
	if err != nil || len(plan.args) != 0 {
		t.Fatalf("compliant live defaults should need no Helm write: %+v %v", plan, err)
	}
}

func TestArgoSecurityDryRunDoesNotConsumeConsent(t *testing.T) {
	deps := fixwaveSecurityDeps(t, false, `{}`)
	input := strings.NewReader("yes\n")
	var output bytes.Buffer
	deps.Input, deps.Output, deps.IsTerminal = input, &output, func() bool { return true }
	cfg := Config{DryRun: true, SkipArgo: true}
	if err := readArgoSecurityReview(t.Context(), &cfg, deps); err == nil || !strings.Contains(err.Error(), "explicit consent") {
		t.Fatalf("dry run should give a remedy: %v", err)
	}
	if input.Len() != len("yes\n") || strings.Contains(output.String(), "[y/N]") {
		t.Fatal("dry run asked for or consumed consent", output.String())
	}
}

func TestArgoSecurityDisabledPodPolicyKeepsDefaultChecks(t *testing.T) {
	deps := fixwaveSecurityDeps(t, false, `{}`)
	values := map[string]any{"argo": map[string]any{"podPolicy": map[string]any{"enabled": false}}}
	if _, err := planUpgradeArgoSecurity(t.Context(), deps, values, false); err == nil || !strings.Contains(err.Error(), "explicit consent") {
		t.Fatalf("upgrade skipped the live default check: %v", err)
	}
	cfg := Config{SkipArgo: true}
	if err := readArgoSecurityReview(t.Context(), &cfg, deps); err == nil || !strings.Contains(err.Error(), "explicit consent") {
		t.Fatalf("install disagrees with upgrade: %v", err)
	}
}

func TestUpgradeDryRunDoesNotConsumeArgoConsent(t *testing.T) {
	fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
	setArgoSecurityLive(t, fixture.deps.KubeClient, DefaultArgoNamespace, false)
	input := strings.NewReader("yes\n")
	fixture.deps.Input, fixture.deps.IsTerminal = input, func() bool { return true }
	original := fixture.deps.RunHelm
	fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 2 && args[0] == "get" && args[2] == "argo-workflows" {
			return []byte(`{}`), nil
		}
		return original(ctx, args)
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true, DryRun: true}, fixture.deps)
	if err == nil || !strings.Contains(err.Error(), "explicit consent") || input.Len() != len("yes\n") {
		t.Fatalf("upgrade dry run consumed consent: unread=%d err=%v", input.Len(), err)
	}
}
