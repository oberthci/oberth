package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestChartReleaseWIFPublicCapabilities(t *testing.T) {
	policy := map[string]any{
		"namespace":    "oberth-argo",
		"roles":        map[string]any{"image-writer": map[string]string{"provider": "projects/123/locations/global/workloadIdentityPools/images/providers/issuer", "service_account": "publisher@example-project.iam.gserviceaccount.com"}},
		"repositories": map[string]any{"github/skipops/oberth": map[string]any{"service_account_name": "oberth-per-repo", "templates": map[string]string{"release-publish-images": "image-writer"}}},
	}
	render := func(p map[string]any) (string, error) {
		t.Helper()
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest}
		if p != nil {
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			args = append(args, "--set-json", "argo.releaseWIF="+string(b))
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		return string(out), err
	}
	disabled, err := render(nil)
	if err != nil {
		t.Fatal(err, disabled)
	}
	if strings.Contains(disabled, "release-wif") {
		t.Fatal("default chart activates federation")
	}
	enabled, err := render(policy)
	if err != nil {
		t.Fatal(err, enabled)
	}
	for _, want := range []string{"name: oberth-release-wif", "capabilities.json:", "checksum/release-wif:", "--argo-release-wif-config=/etc/oberth/release-wif/capabilities.json", "mountPath: /etc/oberth/release-wif"} {
		if !strings.Contains(enabled, want) {
			t.Errorf("missing public capability plumbing %q", want)
		}
	}
	policy["namespace"] = "foreign"
	if out, err := render(policy); err == nil || !strings.Contains(out, "must exactly match") {
		t.Fatal("foreign namespace accepted", out)
	}
	policy["namespace"] = "oberth-argo"
	policy["private_key"] = "forbidden"
	if out, err := render(policy); err == nil {
		t.Fatal("private credential field accepted", out)
	}
	delete(policy, "private_key")
	policy["roles"].(map[string]any)["image-writer"].(map[string]string)["provider"] = "https://attacker.invalid"
	if out, err := render(policy); err == nil {
		t.Fatal("arbitrary provider endpoint accepted", out)
	}
}
