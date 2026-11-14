package installer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/client"
)

func TestUpgradeWaitChecksRecordedClusterBeforeReads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	root, err := client.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{ContextName: "recorded", RestConfig: &rest.Config{Host: "https://recorded.invalid"}, ClientConfigRoot: func() (string, error) { return root, nil }}
	if err := recordKubernetesTarget(Config{Namespace: "oberth"}, deps); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		_, _ = io.WriteString(w, upgradeWorkflowList())
	}))
	defer server.Close()
	deps.RestConfig = &rest.Config{Host: server.URL}
	deps.RunHelm = func(context.Context, []string) ([]byte, error) {
		t.Fatal("changed recorded cluster reached Helm namespace lookup")
		return nil, nil
	}
	err = waitForUpgradeIdle(t.Context(), UpgradeConfig{Namespace: "oberth", WaitTimeout: time.Second}, deps)
	if err == nil || !strings.Contains(err.Error(), "recorded Kubernetes cluster identity changed") || !strings.Contains(err.Error(), "upgrade not attempted") || reads.Load() != 0 {
		t.Fatalf("recorded-cluster refusal: reads=%d err=%v", reads.Load(), err)
	}
}

func TestUpgradeRetryWaitFlags(t *testing.T) {
	for _, tc := range []struct {
		cfg        UpgradeConfig
		wait, skip bool
	}{
		{cfg: UpgradeConfig{WaitTimeout: DefaultUpgradeWaitTimeout}},
		{cfg: UpgradeConfig{WaitTimeout: 30 * time.Minute}, wait: true},
		{cfg: UpgradeConfig{WaitTimeout: 30 * time.Minute, NoWaitIdle: true}, wait: true, skip: true},
	} {
		retry := upgradeRetryCommand(tc.cfg, Deps{ContextName: "selected"})
		if strings.Contains(retry, "--wait-for 30m0s") != tc.wait || strings.Contains(retry, "--no-wait-idle") != tc.skip || strings.Contains(retry, "--wait-timeout") || strings.Contains(retry, "--wait-idle") {
			t.Fatal("noncanonical retry flags", retry)
		}
	}
}
