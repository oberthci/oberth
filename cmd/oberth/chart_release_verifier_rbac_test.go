package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Issue #478: preflight must mint tokens for the release identities provisioned
// by the installer, without granting arbitrary ServiceAccount impersonation.
func TestChartReleaseVerifierTokenRequestRBAC(t *testing.T) {
	for _, tc := range []struct {
		name, serverNamespace, pipelineNamespace, serverAccount string
		values                                                  []string
		wantAccounts                                            []string
	}{
		{
			name: "default shared identity", serverNamespace: "default", pipelineNamespace: "oberth-argo", serverAccount: "oberth",
			wantAccounts: []string{"oberth-argo-credentialed"},
		},
		{
			name: "legacy absent list", serverNamespace: "default", pipelineNamespace: "oberth-argo", serverAccount: "oberth",
			values: []string{"argo.perRepoIdentities=null"}, wantAccounts: []string{"oberth-argo-credentialed"},
		},
		{
			name: "explicit release identities", serverNamespace: "server-system", pipelineNamespace: "pipeline-system", serverAccount: "alternate-oberth",
			values: []string{
				"argo.namespace=pipeline-system", "argo.credentialedServiceAccount=shared-release",
				"argo.perRepoIdentities[0]=oberth-argo-repo-one-123456", "argo.perRepoIdentities[1]=oberth-argo-repo-two-abcdef",
				"argo.perRepoCIIdentities[0]=oberth-argo-ci-repo-one-654321",
			},
			wantAccounts: []string{"shared-release", "oberth-argo-repo-one-123456", "oberth-argo-repo-two-abcdef"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", tc.serverAccount, "../../charts/oberth", "--namespace", tc.serverNamespace,
				"--set", "image.ref=example.invalid/oberth@" + testImageDigest}
			for _, value := range tc.values {
				args = append(args, "--set", value)
			}
			rendered, err := exec.Command("helm", args...).Output()
			if err != nil {
				t.Fatalf("render chart: %v", err)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
			tokenRules, serverBindings := 0, 0
			for {
				var raw json.RawMessage
				if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(raw) == 0 {
					continue
				}
				var role rbacv1.Role
				if err := json.Unmarshal(raw, &role); err != nil {
					t.Fatal(err)
				}
				if role.Kind == "Role" || role.Kind == "ClusterRole" {
					for _, rule := range role.Rules {
						if !slices.Contains(rule.Resources, "serviceaccounts/token") && !slices.Contains(rule.Resources, "*") {
							continue
						}
						tokenRules++
						if role.Kind != "Role" || role.Name != tc.serverAccount+"-argo" || role.Namespace != tc.pipelineNamespace {
							t.Fatalf("token authority escaped pipeline Role: %s %s/%s", role.Kind, role.Namespace, role.Name)
						}
						if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"serviceaccounts/token"}) ||
							!slices.Equal(rule.Verbs, []string{"create"}) || !slices.Equal(rule.ResourceNames, tc.wantAccounts) {
							t.Fatalf("token rule = %+v; want create only for exact release accounts %v", rule, tc.wantAccounts)
						}
					}
				}
				if role.Kind == "RoleBinding" || role.Kind == "ClusterRoleBinding" {
					var binding rbacv1.RoleBinding
					if err := json.Unmarshal(raw, &binding); err != nil {
						t.Fatal(err)
					}
					if binding.RoleRef.Name != tc.serverAccount+"-argo" {
						continue
					}
					serverBindings++
					wantSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: tc.serverAccount, Namespace: tc.serverNamespace}
					wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: tc.serverAccount + "-argo"}
					if binding.Kind != "RoleBinding" || binding.Namespace != tc.pipelineNamespace || !slices.Equal(binding.Subjects, []rbacv1.Subject{wantSubject}) || binding.RoleRef != wantRef {
						t.Fatalf("token Role must bind only the configured server identity: %+v", binding)
					}
				}
			}
			if tokenRules != 1 || serverBindings != 1 {
				t.Fatalf("got %d token rules and %d server bindings, want exactly one each", tokenRules, serverBindings)
			}
		})
	}
}
