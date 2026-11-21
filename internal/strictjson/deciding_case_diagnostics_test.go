package strictjson

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestCaseInsensitiveTypeRefusalKeepsAuthoredPath(t *testing.T) {
	for _, tc := range []struct{ name, input, path string }{
		{"root", `{"LIMIT":"private-value"}`, "LIMIT"},
		{"nested", `{"ITEMS":[{"Count":"private-value"}]}`, "ITEMS[0].Count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := struct {
				Limit int `json:"limit"`
				Items []struct {
					Count int `json:"count"`
				} `json:"items"`
			}{Limit: 17}
			err := DecodeStrictObject([]byte(tc.input), &value, 1024)
			if err == nil {
				t.Fatal("invalid integer accepted")
			}
			problem := publicerr.Classify(err)
			detail := `field "` + tc.path + `": expected an integer, got string`
			if problem.Code() != publicerr.InvalidInput || problem.Reason() != "A JSON field has the wrong type or range." || problem.Remedy() != "Use the expected field type and range." || problem.Detail != detail {
				t.Fatalf("wrong field recovery: %v; detail %q", problem, problem.Detail)
			}
			if value.Limit != 17 || value.Items != nil {
				t.Fatal("refusal changed destination")
			}
		})
	}
}
