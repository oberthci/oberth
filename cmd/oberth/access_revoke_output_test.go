package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

type revokeOutputCase struct{ name, remedy, detail string }

func TestAccessRevokeShowsPublicPartialOutcome(t *testing.T) {
	for _, scenario := range []revokeOutputCase{
		{"sealed", "Fix the store problem in detail, then repeat the revoke.", "Secret store is sealed.\nAn administrator runs oberth unseal, then retry."},
		{"unreachable", "Fix the store problem in detail, then repeat the revoke.", "Secret store connection failed.\nCheck the store address, certificate and readiness, then retry."},
		{"role", "Fix the store problem in detail, then repeat the revoke.", "Secret store refused the request.\nCheck the configured role, account, policy and store state."},
		{"unconfigured", "Configure the secret store writer, then repeat the revoke.", "Secret store writer is not configured."},
		{"audit", "Restore audit access, then repeat the revoke to delete the value.", ""},
	} {
		for _, mode := range []struct {
			name  string
			flags []string
		}{{"human", nil}, {"detail", []string{"--detail"}}, {"json", []string{"-o", "json"}}} {
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) { checkRevokePartialOutput(t, scenario, mode.flags) })
		}
	}
}
func accessRevokeCLIArgs() []string {
	return []string{"access", "revoke", "origin/team/widget", "--workflow", ".oberth/build.yaml", "--step", "build", "--secret", "registry/key"}
}
func checkRevokePartialOutput(t *testing.T, scenario revokeOutputCase, flags []string) {
	t.Helper()
	refusal := publicerr.New(publicerr.Unavailable, "Grant revoked; secret still stored.", scenario.remedy, errors.New("private-store-response"))
	refusal.Detail = scenario.detail
	bindings := fake.Bindings()
	handler := &fake.Handler[apicontract.AccessRevokeArgs, apicontract.Receipt]{Error: refusal}
	bindings.AccessRevoke.Handler = handler
	app, out, diagnostic := fixture(t, bindings)
	if code := app.run(t.Context(), append(accessRevokeCLIArgs(), flags...)); code != 1 {
		t.Fatal("partial revoke exit", code, diagnostic.String())
	}
	input, _, calls := handler.Observed()
	want := apicontract.AccessRevokeArgs{Repo: "origin/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}
	if calls != 1 || len(input) != 1 {
		t.Fatal("revoke dispatch changed", input, calls)
	}
	if input[0] != want {
		t.Fatal("revoke selectors changed", input, calls)
	}
	if strings.Contains(out.String()+diagnostic.String(), "private-store-response") {
		t.Fatal("private cause exposed")
	}
	if len(flags) == 2 {
		assertRevokeJSON(t, scenario, out, diagnostic)
		return
	}

	expected := refusal.Reason() + "\n" + scenario.remedy + "\n"
	if scenario.detail != "" {
		expected += scenario.detail + "\n"
	}
	if out.Len() != 0 || diagnostic.String() != expected {
		t.Error("public partial outcome incomplete or repeated", diagnostic.String())
	}
}
func assertRevokeJSON(t *testing.T, scenario revokeOutputCase, out, diagnostic *bytes.Buffer) {
	t.Helper()
	var wire struct{ Reason, Remedy, Detail string }
	if err := json.Unmarshal(out.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	want := struct{ Reason, Remedy, Detail string }{"Grant revoked; secret still stored.", scenario.remedy, scenario.detail}
	if wire != want || diagnostic.Len() != 0 {
		t.Fatal("JSON refusal changed", out.String(), diagnostic.String())
	}
}
func TestAccessRevokeSuccessAndDismissalRefusal(t *testing.T) {
	bindings := fake.Bindings()
	bindings.AccessRevoke.Handler = &fake.Handler[apicontract.AccessRevokeArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "grant", State: "revoked"}}
	bindings.AccessDismiss.Handler = &fake.Handler[apicontract.AccessDismissArgs, apicontract.Receipt]{Error: publicerr.New(publicerr.InvalidInput, "Only pending requests can be dismissed.", "Use oberth access revoke for an approved or revoked grant.", nil)}
	app, out, diagnostic := fixture(t, bindings)
	if code := app.run(t.Context(), accessRevokeCLIArgs()); code != 0 || !strings.Contains(out.String(), "revoked") || diagnostic.Len() != 0 {
		t.Fatal("successful revoke", code, out.String(), diagnostic.String())
	}
	out.Reset()
	args := []string{"access", "dismiss", "--repo", "origin/team/widget", "--workflow", ".oberth/build.yaml", "--step", "build", "--secret", "registry/key"}
	if code := app.run(t.Context(), args); code != 1 || out.Len() != 0 || diagnostic.String() != "Only pending requests can be dismissed.\nUse oberth access revoke for an approved or revoked grant.\n" {
		t.Fatal("dismissal refusal changed", code, out.String(), diagnostic.String())
	}
}
