package installer

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/client"
)

func TestUpgradeUnconfiguredAdminRefusesBeforeMutation(t *testing.T) {
	for _, version := range []string{"0.22.4", "0.22.6", "0.22.7"} {
		for _, noBackup := range []bool{false, true} {
			for _, dry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/no-backup=%t/dry=%t", version, noBackup, dry), func(t *testing.T) { testUpgradeUnconfiguredAdmin(t, version, noBackup, dry) })
			}
		}
	}
}

func testUpgradeUnconfiguredAdmin(t *testing.T, version string, noBackup, dry bool) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	f := newUpgradeFixture(t, version, "0.22.8", "99")
	f.deps.Admin = nil
	f.deps.ClientConfigRoot = func() (string, error) { return t.TempDir(), nil }
	writes, commands := 0, 0
	helm := f.deps.RunHelm
	f.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			writes++
		}
		return helm(ctx, args)
	}
	f.deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		commands++
		return nil, fmt.Errorf("unexpected host command")
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.8", Namespace: DefaultNamespace, NoBackup: noBackup, DryRun: dry}, f.deps)
	for _, action := range f.deps.KubeClient.(*fake.Clientset).Actions() {
		if slices.Contains([]string{"create", "update", "patch", "delete"}, action.GetVerb()) {
			t.Fatal("Kubernetes write before prerequisite refusal", action.GetVerb())
		}
	}
	if err == nil || writes != 0 || commands != 0 {
		t.Fatal("unconfigured client reached a write", writes, commands, err)
	}
	if !noBackup && version != "0.22.7" {
		for _, want := range []string{"./oberth-v" + version + " backup --history-only --context fixture --namespace oberth", "oberth upgrade --no-backup", "not copied"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatal("missing exact backup guidance", want, err)
			}
		}
	} else {
		for _, want := range []string{"unscoped admin", "export OBERTH_BASE_URL=", "OBERTH_CLIENT_CERT=", "OBERTH_CLIENT_KEY=", "OBERTH_CA_CERT=", "nothing changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatal("missing client prerequisite", want, err)
			}
		}
	}
}

func TestUpgradeMissingCurrentAdminAPIGivesRetryCommand(t *testing.T) {
	deps := testAdminDeps(Deps{ContextName: "selected"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	deps.Admin = remote
	err = checkUpgradeBackupAPI(t.Context(), UpgradeConfig{Namespace: DefaultNamespace, DryRun: true}, deps, "0.22.7")
	if err == nil || !strings.Contains(err.Error(), "oberth upgrade --no-backup --context selected --namespace oberth") || !strings.Contains(err.Error(), "older Pods") {
		t.Fatal("failed-release guidance missing", err)
	}
	var output bytes.Buffer
	_, err = printUpgradeDryRun(&output, UpgradeConfig{WaitTimeout: time.Minute}, UpgradeResult{PreviousVersion: "0.22.7", TargetVersion: "0.22.8"}, false)
	if err != nil || !strings.Contains(output.String(), "active Workflows") || strings.Contains(output.String(), "live daemon queue") {
		t.Fatal("dry-run describes the wrong observer", err, output.String())
	}
}
