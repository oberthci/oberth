package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"testing"
	"time"
)

func TestRegistryRefusesDuplicateAndUnclassifiedTools(t *testing.T) {
	h := apicontract.HandlerFunc[apicontract.Empty, apicontract.Receipt](func(context.Context, apicontract.Caller, apicontract.Empty) (apicontract.Call[apicontract.Receipt], error) {
		return func(context.Context) (apicontract.Receipt, error) { return apicontract.Receipt{}, nil }, nil
	})
	m := apicontract.Metadata{Name: "operation", Description: "Observe an operation.", Access: apicontract.Read, InputBytes: apicontract.RequestBytes, OutputBytes: apicontract.ResponseBytes, Deadline: time.Second}
	entry := bindDefinition(apicontract.Binding[apicontract.Empty, apicontract.Receipt]{Handler: h}.Definition(m))
	if _, err := NewRegistry([]Tool{entry, entry}); err == nil {
		t.Fatal("duplicate declaration accepted")
	}
	m.Access = ""
	if _, err := NewRegistry([]Tool{bindDefinition(apicontract.Binding[apicontract.Empty, apicontract.Receipt]{Handler: h}.Definition(m))}); err == nil {
		t.Fatal("unclassified tool accepted")
	}
}
func TestDiagnosticFrameCountSaturates(t *testing.T) {
	v := uint16(65534)
	countFrame(&v)
	if v != 65535 {
		t.Fatal("last frame was not counted")
	}
	countFrame(&v)
	if v != 65535 {
		t.Fatal("frame count wrapped")
	}
}

func TestSchemasDescribeCustomPublicErrorAndRawDataValues(t *testing.T) {
	s := schemaForType[publicerr.Error]()
	properties := s["properties"].(apicontract.Schema)
	for _, field := range []string{"id", "code", "reason", "remedy", "detail"} {
		if properties[field].(apicontract.Schema)["type"] != "string" {
			t.Fatal("public error schema differs from its wire value")
		}
	}
	if len(schemaForType[json.RawMessage]()) != 0 {
		t.Fatal("raw event data acquired an invented wire shape")
	}
}
