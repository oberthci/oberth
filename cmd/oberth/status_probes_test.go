package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
)

func TestStatusProbeFlagsAndChart(t *testing.T) {
	binary, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || binary.statusProbes != app.DefaultStatusProbeConfig() {
		t.Fatal(binary.statusProbes, err)
	}
	for _, tc := range []struct {
		sets []string
		want app.StatusProbeConfig
	}{
		{nil, app.DefaultStatusProbeConfig()},
		{[]string{"statusProbes.upstreamIntervalSeconds=90", "statusProbes.clusterIntervalSeconds=120", "statusProbes.secretStoreIntervalSeconds=180", "statusProbes.upstreamFailureThreshold=4"}, app.StatusProbeConfig{UpstreamInterval: 90 * time.Second, ClusterInterval: 120 * time.Second, SecretStoreInterval: 180 * time.Second, FailureThreshold: 4}},
	} {
		args, err := renderSchedulingChart(t, tc.sets...)
		if err != nil {
			t.Fatal(err)
		}
		options, err := parseServeOptions(args, io.Discard)
		if err != nil || options.statusProbes != tc.want {
			t.Fatal(options.statusProbes, tc.want, err)
		}
		health := app.Health{}
		configureStatusProbes(&health, options)
		if health.StatusProbes != tc.want {
			t.Fatal("serve did not wire probe configuration")
		}
	}
	for _, field := range []string{"status-upstream-probe-interval", "status-cluster-probe-interval", "status-secretstore-probe-interval"} {
		for _, value := range []string{"0", "-1s", "9s", "61m"} {
			if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--" + field + "=" + value}, io.Discard); err == nil {
				t.Fatal("accepted invalid probe interval", field, value)
			}
		}
	}
	for _, value := range []string{"0", "-1", "11"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--status-upstream-failure-threshold=" + value}, io.Discard); err == nil {
			t.Fatal("accepted failure threshold", value)
		}
	}
	for _, set := range []string{"statusProbes.upstreamIntervalSeconds=0", "statusProbes.clusterIntervalSeconds=3601", "statusProbes.secretStoreIntervalSeconds=9", "statusProbes.upstreamFailureThreshold=0", "statusProbes.upstreamFailureThreshold=11"} {
		if _, err := renderSchedulingChart(t, set); err == nil {
			t.Fatal("schema accepted invalid setting", set)
		}
	}
}

func TestStatusProbeIdentityUsesResolvedCredentialsAndPins(t *testing.T) {
	dir := t.TempDir()
	options := serveOptions{upstreamKey: filepath.Join(dir, "shared"), knownHosts: filepath.Join(dir, "known_hosts")}
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(options.upstreamKey, "shared credential")
	write(options.knownHosts, "pins")
	health := app.Health{}
	configureStatusProbes(&health, options)
	identity := func(key string) string {
		t.Helper()
		value, probe, err := health.PrepareVCS(t.Context(), model.Upstream{Kind: "ssh", BaseURL: "ssh://git@forge.example/org", KeyName: key})
		if err != nil || probe == nil {
			t.Fatal(err)
		}
		return value
	}
	shared := identity("")
	if identity("dedicated") != shared {
		t.Fatal("missing dedicated key did not use shared-key binding")
	}
	write(filepath.Join(dir, "dedicated"), "dedicated credential")
	dedicated := identity("dedicated")
	if dedicated == shared {
		t.Fatal("different credentials shared a probe binding")
	}
	write(filepath.Join(dir, "dedicated"), "rotated credential")
	if identity("dedicated") == dedicated {
		t.Fatal("credential rotation retained old probe binding")
	}
	write(options.knownHosts, "new pins")
	if identity("") == shared {
		t.Fatal("pin rotation retained old observation")
	}
}
