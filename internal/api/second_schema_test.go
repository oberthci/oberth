package api_test

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"reflect"
	"strings"
	"testing"
)

func TestSecondEveryToolSchema(t *testing.T) {
	f := setup(t, nil)
	assertSuccess(t, discover(f))
	reg, _ := api.Builtin(f.bindings)
	for _, m := range listTools(t, reg, admin()) {
		w := rpc(f, m.Name, toolArgs(m))
		assertSuccess(t, w)
		var wire map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &wire)
		value := wire["result"].(map[string]any)["structuredContent"]
		raw, _ := json.Marshal(m.OutputSchema)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		errs := secondSchemaErrors(schema, value, "$")
		if len(errs) > 0 {
			t.Errorf("%s: %v", m.Name, errs)
		}
	}
}

func TestSecondPopulateSchemas(t *testing.T) {
	// Generate fully populated output values from every declared output schema,
	// decode to the bound concrete result type, then run the real transport and compare.
	f := setup(t, nil)
	reg, _ := api.Builtin(f.bindings)
	bv := reflect.ValueOf(f.bindings)
	for _, m := range listTools(t, reg, admin()) {
		out := secondOutputSlot(bv, m.Name)
		if !out.IsValid() {
			t.Fatal("missing fixture", m.Name)
		}
		data, _ := json.Marshal(secondSchemaSample(m.OutputSchema))
		ptr := reflect.New(out.Type())
		if err := json.Unmarshal(data, ptr.Interface()); err != nil {
			t.Errorf("%s generated result decode: %v", m.Name, err)
			continue
		}
		out.Set(ptr.Elem())
	}
	assertSuccess(t, discover(f))
	for _, m := range listTools(t, reg, admin()) {
		w := rpc(f, m.Name, toolArgs(m))
		var wire map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if wire["error"] != nil || wire["result"].(map[string]any)["isError"] == true {
			t.Fatalf("%s refusal: %s", m.Name, w.Body.String())
		}
		value := wire["result"].(map[string]any)["structuredContent"]
		raw, _ := json.Marshal(m.OutputSchema)
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if errs := secondSchemaErrors(schema, value, "$"); len(errs) > 0 {
			t.Errorf("%s: %v", m.Name, errs)
		}
	}
}
func secondSchemaSample(s apicontract.Schema) any {
	if variants, ok := s["anyOf"].([]apicontract.Schema); ok {
		return secondSchemaSample(variants[0])
	}
	switch s["type"] {
	case "object":
		m := map[string]any{}
		if props, ok := s["properties"].(apicontract.Schema); ok {
			for k, v := range props {
				m[k] = secondSchemaSample(v.(apicontract.Schema))
			}
		}
		return m
	case "array":
		return []any{secondSchemaSample(s["items"].(apicontract.Schema))}
	case "string":
		if s["format"] == "date-time" {
			return "2026-01-01T00:00:00Z"
		}
		return "x"
	case "boolean":
		return true
	case "integer":
		return 1
	}
	return nil
}

func secondOutputSlot(bv reflect.Value, name string) reflect.Value {
	for _, field := range reflect.VisibleFields(bv.Type()) {
		if field.Anonymous {
			continue
		}
		if strings.EqualFold(field.Name, strings.ReplaceAll(name, "_", "")) {
			return bv.FieldByIndex(field.Index).FieldByName("Handler").Elem().Elem().FieldByName("Value")
		}
	}
	return reflect.Value{}
}

func TestRefusalContextMatchesPublishedSchema(t *testing.T) {
	problem := publicerr.New(publicerr.Forbidden, "Host is not trusted.", "Verify its fingerprint.", nil)
	problem.Context = []string{"Offered fingerprint: SHA256:example"}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.IssueGetMany.Handler = &fake.Handler[apicontract.IssueGetManyArgs, apicontract.IssueManyResult]{Value: apicontract.IssueManyResult{Issues: []apicontract.IssueResult{{ID: 1, Error: problem}}}}
	})
	reg, _ := api.Builtin(f.bindings)
	w := rpc(f, "issue_get_many", map[string]any{"ids": []int{1}})
	var wire map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	result := wire["result"].(map[string]any)
	if w.Code != 200 || wire["error"] != nil || result["isError"] != false {
		t.Fatal("unexpected RPC envelope", w.Body.String())
	}
	value := result["structuredContent"]
	if !strings.Contains(w.Body.String(), "Offered fingerprint: SHA256:example") {
		t.Fatal("refusal context lost", w.Body.String())
	}
	for _, m := range listTools(t, reg, admin()) {
		if m.Name != "issue_get_many" {
			continue
		}
		raw, _ := json.Marshal(m.OutputSchema)
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if errs := secondSchemaErrors(schema, value, "$"); len(errs) > 0 {
			t.Fatal(errs)
		}
		return
	}
	t.Fatal("missing tool schema")
}
