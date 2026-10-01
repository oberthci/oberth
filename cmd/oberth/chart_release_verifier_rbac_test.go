package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Issue #478: preflight must mint tokens for the release identities provisioned
// by the installer, without granting arbitrary ServiceAccount impersonation.
func TestChartReleaseVerifierTokenRequestRBAC(t *testing.T) {
	var defaultRules []rbacv1.PolicyRule
	for _, tc := range []struct {
		name, serverNamespace, pipelineNamespace, serverAccount string
		values                                                  []string
		wantShared, wantPerRepo                                 []string
	}{
		{
			name: "default shared identity", serverNamespace: "default", pipelineNamespace: "oberth-argo", serverAccount: "oberth",
			wantShared: []string{"oberth-argo-credentialed"},
		},
		{
			name: "legacy absent list", serverNamespace: "default", pipelineNamespace: "oberth-argo", serverAccount: "oberth",
			values: []string{"argo.perRepoIdentities=null"}, wantShared: []string{"oberth-argo-credentialed"},
		},
		{
			name: "per-repo release identities leave existing Role unchanged", serverNamespace: "default", pipelineNamespace: "oberth-argo", serverAccount: "oberth",
			values: []string{
				"argo.perRepoIdentities[0]=oberth-argo-repo-one-123456", "argo.perRepoIdentities[1]=oberth-argo-repo-two-abcdef",
				"argo.perRepoCIIdentities[0]=oberth-argo-ci-repo-one-654321",
			},
			wantShared:  []string{"oberth-argo-credentialed"},
			wantPerRepo: []string{"oberth-argo-repo-one-123456", "oberth-argo-repo-two-abcdef"},
		},
		{
			name: "explicit release identities", serverNamespace: "server-system", pipelineNamespace: "pipeline-system", serverAccount: "alternate-oberth",
			values: []string{
				"argo.namespace=pipeline-system", "argo.credentialedServiceAccount=shared-release",
				"argo.perRepoIdentities[0]=oberth-argo-repo-one-123456", "argo.perRepoIdentities[1]=oberth-argo-repo-two-abcdef",
				"argo.perRepoCIIdentities[0]=oberth-argo-ci-repo-one-654321",
			},
			wantShared:  []string{"shared-release"},
			wantPerRepo: []string{"oberth-argo-repo-one-123456", "oberth-argo-repo-two-abcdef"},
		},
	} {
		// The default and populated render must keep the established Role's
		// entire rules list equal, so Helm never applies a change to co-owned
		// .rules when the installer provisions more release identities.
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
			tokenRules := 0
			roles := make(map[string]rbacv1.Role)
			bindings := make(map[string]rbacv1.RoleBinding)
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
					if role.Kind == "Role" && role.Namespace == tc.pipelineNamespace {
						roles[role.Name] = role
					}
					for _, rule := range role.Rules {
						if !slices.Contains(rule.Resources, "serviceaccounts/token") && !slices.Contains(rule.Resources, "*") {
							continue
						}
						tokenRules++
						if role.Kind != "Role" || role.Namespace != tc.pipelineNamespace {
							t.Fatalf("token authority escaped pipeline Role: %s %s/%s", role.Kind, role.Namespace, role.Name)
						}
						wantNames := tc.wantShared
						if role.Name == tc.serverAccount+"-argo-release-tokens" {
							wantNames = tc.wantPerRepo
						} else if role.Name != tc.serverAccount+"-argo" {
							t.Fatalf("unexpected token Role %s", role.Name)
						}
						if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"serviceaccounts/token"}) ||
							!slices.Equal(rule.Verbs, []string{"create"}) || !slices.Equal(rule.ResourceNames, wantNames) {
							t.Fatalf("token rule = %+v; want create only for exact release accounts %v", rule, wantNames)
						}
					}
				}
				if role.Kind == "RoleBinding" || role.Kind == "ClusterRoleBinding" {
					var binding rbacv1.RoleBinding
					if err := json.Unmarshal(raw, &binding); err != nil {
						t.Fatal(err)
					}
					if binding.Kind == "RoleBinding" && binding.Namespace == tc.pipelineNamespace {
						bindings[binding.Name] = binding
					}
				}
			}
			wantRoleNames := []string{tc.serverAccount + "-argo"}
			if len(tc.wantPerRepo) > 0 {
				wantRoleNames = append(wantRoleNames, tc.serverAccount+"-argo-release-tokens")
			}
			if tokenRules != len(wantRoleNames) {
				t.Fatalf("got %d token rules, want %d", tokenRules, len(wantRoleNames))
			}
			for _, name := range wantRoleNames {
				role, ok := roles[name]
				if !ok {
					t.Fatalf("missing Role %s", name)
				}
				if name == tc.serverAccount+"-argo-release-tokens" && len(role.Rules) != 1 {
					t.Fatalf("per-repo Role has %d rules, want only TokenRequest", len(role.Rules))
				}
				binding, ok := bindings[name]
				if !ok {
					t.Fatalf("missing RoleBinding %s", name)
				}
				wantSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: tc.serverAccount, Namespace: tc.serverNamespace}
				wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name}
				if !slices.Equal(binding.Subjects, []rbacv1.Subject{wantSubject}) || binding.RoleRef != wantRef {
					t.Fatalf("RoleBinding %s does not bind only the server identity: %+v", name, binding)
				}
			}
			if len(tc.wantPerRepo) == 0 && (roles[tc.serverAccount+"-argo-release-tokens"].Name != "" || bindings[tc.serverAccount+"-argo-release-tokens"].Name != "") {
				t.Fatal("empty per-repo list created a token Role or binding")
			}
			if tc.serverAccount == "oberth" && slices.Equal(tc.wantShared, []string{"oberth-argo-credentialed"}) {
				if defaultRules == nil {
					defaultRules = roles["oberth-argo"].Rules
				} else if !reflect.DeepEqual(defaultRules, roles["oberth-argo"].Rules) {
					t.Fatal("per-repo identities changed the established Argo Role rules")
				}
			}
		})
	}
}
