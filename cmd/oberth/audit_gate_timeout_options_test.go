package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestAuditGateDeadlineOptions(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || defaults.auditGateTimeout != 2*time.Minute || defaults.auditGateVerifyTimeout != 45*time.Second {
		t.Fatalf("audit gate defaults: total=%s attempt=%s err=%v", defaults.auditGateTimeout, defaults.auditGateVerifyTimeout, err)
	}
	configured, err := parseServeOptions([]string{"--argo-namespace=argo", "--audit-gate-timeout=3m", "--audit-gate-verify-timeout=50s"}, io.Discard)
	if err != nil || configured.auditGateTimeout != 3*time.Minute || configured.auditGateVerifyTimeout != 50*time.Second {
		t.Fatalf("configurable audit gate deadlines: %v", err)
	}
	for _, flag := range []string{"audit-gate-timeout", "audit-gate-verify-timeout"} {
		for _, value := range []string{"0", "-1s", "bad"} {
			if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--" + flag + "=" + value}, io.Discard); err == nil {
				t.Errorf("accepted invalid %s=%s", flag, value)
			}
		}
	}
}

func TestChartAuditGateDeadlineOptions(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required")
	}
	for _, override := range []bool{false, true} {
		args := []string{"template", "oberth", "../../charts/oberth"}
		want := []string{"--audit-gate-timeout=2m", "--audit-gate-verify-timeout=45s"}
		if override {
			args = append(args, "--set", "auditAnchor.gateTimeout=3m", "--set", "auditAnchor.verifyTimeout=50s")
			want = []string{"--audit-gate-timeout=3m", "--audit-gate-verify-timeout=50s"}
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render audit gate settings: %v\n%s", err, output)
		}
		for _, flag := range want {
			if !strings.Contains(string(output), flag) {
				t.Errorf("rendered chart omits %s", flag)
			}
		}
	}
}
