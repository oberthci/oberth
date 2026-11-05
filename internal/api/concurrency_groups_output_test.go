package api

import (
	"encoding/json"
	"testing"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/validate"
)

func TestConcurrencyGroupsOutputRequiresArray(t *testing.T) {
	body, err := json.Marshal(toolOutputSchema("concurrency_groups"))
	if err != nil {
		t.Fatal(err)
	}
	var schema spec.Schema
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{}`, `{"groups":null}`, `{"groups":{}}`, `{"groups":"invalid"}`, `{"groups":["invalid"]}`} {
		var value any
		if err := json.Unmarshal([]byte(input), &value); err != nil {
			t.Fatal(err)
		}
		if err := validate.AgainstSchema(&schema, value, strfmt.Default); err == nil {
			t.Fatalf("invalid groups contract accepted: %s", input)
		}
	}
}
