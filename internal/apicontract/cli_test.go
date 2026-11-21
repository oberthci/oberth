package apicontract_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestCLIContractGolden(t *testing.T) {
	var rows []cliRow
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		rows = append(rows, cliDeclaration(t, d))
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	const path = "testdata/cli.json"
	if os.Getenv("UPDATE_CLI_GOLDEN") == "1" {
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(data) {
		t.Fatal("CLI declaration golden differs")
	}
}

func TestCredentialSensitivityIsDeclared(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		name string
	}{
		{reflect.TypeFor[apicontract.UpstreamProvideKeyArgs](), "private_key"},
		{reflect.TypeFor[model.InvitationFile](), "secret"},
	} {
		found := false
		for _, f := range apicontract.Arguments(tc.typ) {
			if f.Name == tc.name {
				found = f.Sensitive
			}
		}
		if !found {
			t.Fatalf("missing sensitive declaration %s.%s", tc.typ, tc.name)
		}
	}
}

type cliField struct {
	Name       string
	Sensitive  bool
	Position   int
	Positional bool `json:",omitempty"`
}
type cliRow struct {
	Name, Summary   string
	Command         string
	CommandFlow     bool
	Arguments       []cliField
	SensitiveResult []string
}

func cliDeclaration(t *testing.T, d apicontract.Definition) cliRow {
	t.Helper()
	if len(d.Summary) > 60 || len(strings.Fields(d.Summary)) < 3 || strings.EqualFold(strings.TrimSuffix(d.Summary, "."), strings.ReplaceAll(d.Name, "_", " ")) {
		t.Fatalf("invalid summary: %s %q", d.Name, d.Summary)
	}
	r := cliRow{Name: d.Name, Summary: d.Summary, Command: d.Command, CommandFlow: d.CommandFlow}
	positions := map[int]bool{}
	for _, a := range apicontract.Arguments(d.InputType) {
		for _, tag := range []string{"sensitive", "position"} {
			if _, ok := a.Field.Tag.Lookup(tag); !ok {
				t.Fatalf("missing %s: %s.%s", tag, d.Name, a.Name)
			}
		}
		if a.Position > 0 && (positions[a.Position] || a.Sensitive) {
			t.Fatalf("invalid position: %s.%s", d.Name, a.Name)
		}
		positions[a.Position] = true
		r.Arguments = append(r.Arguments, cliField{a.Name, a.Sensitive, a.Position, a.Positional})
	}
	for _, a := range apicontract.Arguments(d.ResultType) {
		if a.Sensitive {
			r.SensitiveResult = append(r.SensitiveResult, a.Name)
		}
	}
	return r
}

func TestBearerContractExposesOnlyCallerDigest(t *testing.T) {
	found := false
	for _, field := range apicontract.Arguments(reflect.TypeFor[apicontract.UplinkAddArgs]()) {
		if field.Name == "bearer" {
			t.Fatal("server-generated bearer flag returned")
		}
		if field.Name == "bearer_digest" {
			found = true
		}
	}
	if !found {
		t.Fatal("caller bearer digest is absent")
	}
	for _, field := range apicontract.Arguments(reflect.TypeFor[apicontract.Identity]()) {
		if field.Name == "token" {
			t.Fatal("server plaintext bearer output returned")
		}
	}
}
