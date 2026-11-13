package installer

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSyncRemovesOrphanRepoPoliciesAndFailsClosed(t *testing.T) {
	for _, failure := range []string{"", "list", "role", "role-only", "role-list"} {
		t.Run(failure, func(t *testing.T) {
			name := PerRepoName("github", "oberthci", "removed")
			ciName := PerRepoCIName("github", "oberthci", "removed")
			responses := map[string]fakeBaoResponse{
				"policy read " + defaultCredentialedPolicy: {out: OberthCredentialedPolicyWithGrants(defaultKVPrefix,nil)},
				"policy read " + defaultCISecretsPolicy:    {out: OberthCISecretsPolicy(defaultKVPrefix)},
				"policy list":                              {out: "default\nroot\n" + name + "\n" + ciName + "\n"},
				"policy delete " + name:                    {},
				"policy delete " + ciName:                  {},
				"delete auth/kubernetes/role/" + name:      {},
				"delete auth/kubernetes/role/" + ciName:    {},
			}
			responses["list -format=json auth/kubernetes/role"] = fakeBaoResponse{out: `["` + name + `","` + ciName + `"]`}
			if failure == "role-only" {
				responses["policy list"] = fakeBaoResponse{out: "default\nroot\n"}
			}
			if failure == "role-list" {
				responses["list -format=json auth/kubernetes/role"] = fakeBaoResponse{err: errors.New("role list unavailable")}
			}
			if failure == "list" {
				responses["policy list"] = fakeBaoResponse{err: errors.New("list unavailable")}
			}
			if failure == "role" {
				responses["delete auth/kubernetes/role/"+name] = fakeBaoResponse{err: errors.New("role deletion unavailable")}
			}
			runner := &fakeBaoRunner{t: t, responses: responses}
			results, err := syncGrantPolicies(t.Context(), openBaoExec{run: runner.run}, "root", nil, nil, "oberth-argo", io.Discard)
			if failure != "" && failure != "role-only" {
				if err == nil {
					t.Fatalf("sync succeeded despite %s failure: %+v", failure, results)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := runner.callsByCommand()
			for _, name := range []string{name, ciName} {
				for _, command := range []string{"policy delete " + name, "delete auth/kubernetes/role/" + name} {
					if _, ok := calls[command]; !ok {
						t.Errorf("orphan cleanup missed %s", command)
					}
				}
			}
			for command := range calls {
				if strings.Contains(command, "delete default") || strings.Contains(command, "delete root") {
					t.Fatalf("deleted unrelated policy: %s", command)
				}
			}
		})
	}
}
