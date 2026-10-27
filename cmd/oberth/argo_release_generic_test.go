package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
)

func TestReadReleaseTagRoutesStrictAdminConfig(t *testing.T) {
	if routes, err := readArgoReleaseTagRoutes(""); err != nil || routes != nil {
		t.Fatal(routes, err)
	}
	if _, err := readArgoReleaseTagRoutes("relative.json"); err == nil {
		t.Fatal("relative admin path")
	}
	name := filepath.Join(t.TempDir(), "routes.json")
	const valid = `{"github/org/repo":[{"pattern":"deploy-*","file":".oberth/deploy.yaml"}]}`
	if err := os.WriteFile(name, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	routes, err := readArgoReleaseTagRoutes(name)
	if err != nil {
		t.Fatal(err)
	}
	if file, err := routes.Resolve("github/org/repo", "refs/tags/deploy-1"); err != nil || file != ".oberth/deploy.yaml" {
		t.Fatal(file, err)
	}
	for _, body := range []string{`null`, `[]`, `{} {}`, `{"github/org/repo":null}`, `{"github/org/repo":[]}`, `{"github/org/repo":[],"github/org/repo":[]}`, `{"github/org/repo":[{"Pattern":"*","file":".oberth/deploy.yaml"}]}`, `{"github/org/repo":[{"pattern":"*","file":".oberth/deploy.yaml","file":".oberth/other.yaml"}]}`, strings.Repeat(" ", (1<<20)+1)} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readArgoReleaseTagRoutes(name); err == nil {
			t.Fatalf("ambiguous config accepted: %.100s", body)
		}
	}
}

func genericChartWIF() map[string]any {
	return map[string]any{"namespace": "oberth-argo", "roles": map[string]any{"binary-deployer": map[string]string{"provider": "projects/123/locations/global/workloadIdentityPools/deployers/providers/issuer", "service_account": "deployer@example-project.iam.gserviceaccount.com"}}, "repositories": map[string]any{"github/org/repo": map[string]any{"service_account_name": "repo-release", "templates": map[string]string{"publish": "binary-deployer"}, "step_service_accounts": map[string]string{"publish": "repo-publish-only"}}}}
}

func TestChartGenericWIFAndTagRoutes(t *testing.T) {
	policy := genericChartWIF()
	routes := argojob.ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}
	render := func() (string, error) {
		p, _ := json.Marshal(policy)
		r, _ := json.Marshal(routes)
		out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+testImageDigest, "--set-json", "argo.releaseWIF="+string(p), "--set-json", "argo.releaseTagRoutes="+string(r)).CombinedOutput()
		return string(out), err
	}
	out, err := render()
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{"name: repo-publish-only", "oberth.ci/identity-type: release-wif-step", "checksum/release-tag-routes:", "--argo-release-tag-routes-config=/etc/oberth/release-tag-routes/routes.json", "mountPath: /etc/oberth/release-tag-routes", "name: oberth-release-tag-routes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	p, _ := json.Marshal(policy)
	name := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(name, p, 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readArgoReleaseWIFConfig(name)
	if err != nil || parsed.Repositories["github/org/repo"].StepServiceAccounts["publish"] != "repo-publish-only" {
		t.Fatal(parsed, err)
	}
	for _, sa := range []string{"repo-release", "oberth-argo-pipeline", "bad/name"} {
		policy["repositories"].(map[string]any)["github/org/repo"].(map[string]any)["step_service_accounts"] = map[string]string{"publish": sa}
		if out, err := render(); err == nil {
			t.Fatalf("unsafe account %q accepted: %s", sa, out)
		}
	}
	policy = genericChartWIF()
	policy["repositories"].(map[string]any)["github/org/repo"].(map[string]any)["templates"] = map[string]string{"publish": "not-configured"}
	if out, err := render(); err == nil {
		t.Fatal("unconfigured role accepted", out)
	}
	policy = genericChartWIF()
	delete(policy["repositories"].(map[string]any)["github/org/repo"].(map[string]any), "service_account_name")
	if out, err := render(); err != nil {
		t.Fatal("grant-free pinned WIF account required a repository account", err, out)
	}
	policy = genericChartWIF()
	policy["roles"].(map[string]any)["Bad_Role"] = policy["roles"].(map[string]any)["binary-deployer"]
	if out, err := render(); err == nil {
		t.Fatal("invalid DNS role accepted", out)
	}
}
