package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

// Every access argument refusal names the argument a caller passes, never an
// internal grouping such as "grant" or "claim/tier".
func TestAccessRefusalsNameTheArgument(t *testing.T) {
	t.Parallel()
	p := people(t)
	tuple := apicontract.AccessAllowArgs{Repo: repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "walk/value"}
	for _, tc := range []struct {
		name   string
		edit   func(*apicontract.AccessAllowArgs)
		reason string
	}{
		{"store path without workflow", func(q *apicontract.AccessAllowArgs) { q.Workflow = "" }, "This grant kind needs the workflow argument."},
		{"store path without workflow, with tier", func(q *apicontract.AccessAllowArgs) { q.Workflow, q.Tier = "", "ci" }, "This grant kind needs the workflow argument."},
		{"store path with tier", func(q *apicontract.AccessAllowArgs) { q.Tier = "ci" }, "This grant kind takes no tier argument."},
		{"store path with audience", func(q *apicontract.AccessAllowArgs) { q.Audience = "aud" }, "This grant kind takes no audience argument."},
		{"identity without audience", func(q *apicontract.AccessAllowArgs) { q.Kind, q.Secret = "identity", "identity" }, "This grant kind needs the audience argument."},
		{"pvc without tier", func(q *apicontract.AccessAllowArgs) {
			*q = apicontract.AccessAllowArgs{Repo: repository, Kind: "pvc", Claim: "data"}
		}, "This grant kind needs the tier argument."},
		{"pvc with step", func(q *apicontract.AccessAllowArgs) {
			*q = apicontract.AccessAllowArgs{Repo: repository, Kind: "pvc", Claim: "data", Tier: "ci", Step: "build"}
		}, "This grant kind takes no step argument."},
		{"pvc with another tier", func(q *apicontract.AccessAllowArgs) {
			*q = apicontract.AccessAllowArgs{Repo: repository, Kind: "pvc", Claim: "data", Tier: "prod"}
		}, "Invalid field: tier."},
		{"unclean workflow", func(q *apicontract.AccessAllowArgs) { q.Workflow = "../build.yaml" }, "Invalid field: workflow."},
		{"inexact secret", func(q *apicontract.AccessAllowArgs) { q.Secret = "/walk/value" }, "Invalid field: secret."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := tuple
			tc.edit(&q)
			for _, revoke := range []bool{false, true} {
				var e error
				if revoke {
					_, e = p.f.s.PrepareAccessRevoke(t.Context(), adminCaller(), apicontract.AccessRevokeArgs(q))
				} else {
					_, e = p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), q)
				}
				requireField(t, e, tc.reason)
				if strings.Contains(e.Error(), "claim/tier") || strings.Contains(e.Error(), "field: grant") {
					t.Fatal("refusal names an internal field", e)
				}
			}
		})
	}
}
