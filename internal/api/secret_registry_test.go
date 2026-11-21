package api

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSecretWriterRegistryKeepsValueOutOfArguments(t *testing.T) {
	var declaration apicontract.Definition
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name == "secret_put" {
			declaration = d
		}
	}
	m := declaration.Metadata
	if !m.Mutation || m.Access != apicontract.Admin || m.Post != "/api/secrets/put" || m.Get != "" || m.InputBytes != apicontract.SecretBodyBytes {
		t.Fatal("secret writer authority or transport differs")
	}
	schema := schemaFor(declaration.InputType)
	if _, ok := schema["properties"].(apicontract.Schema)["value"]; ok {
		t.Fatal("private value is a tool argument")
	}
	declaration.Bound = true
	declaration.Prepare = func(context.Context, apicontract.Caller, any) (apicontract.Call[any], error) {
		t.Fatal("private argument reached handler")
		return nil, nil
	}
	r, err := NewRegistry([]Tool{bindDefinition(declaration)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Invoke(t.Context(), apicontract.Caller{Actor: actor.Actor{Admin: true}}, "secret_put", []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key","value":"private"}`))
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("private argument accepted or echoed")
	}
}
