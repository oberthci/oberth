package api_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestMCPRefusesInvalidUTF8BeforePreparation(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"invalid byte", string([]byte{0xff}), false},
		{"unfinished rune", string([]byte{0xc3}), false},
		{"overlong encoding", string([]byte{0xc0, 0xaf}), false},
		{"valid replacement rune", "\uFFFD", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_get","arguments":{"id":"12345678` + tc.value + `"}}}`)
			w := request(f, "POST", "/mcp", body)
			h := f.bindings.RunGet.Handler.(*fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary])
			inputs, _, calls := h.Observed()
			if tc.valid {
				assertSuccess(t, w)
				if len(inputs) != 1 || calls != 1 || inputs[0].ID != "12345678"+tc.value {
					t.Fatal("valid UTF-8 changed before execution")
				}
				return
			}
			if len(inputs) != 0 || calls != 0 {
				t.Fatal("invalid UTF-8 reached preparation or execution", len(inputs), calls)
			}
			assertInvalidUTF8Refusal(t, w.Body.Bytes())
		})
	}
}

func assertInvalidUTF8Refusal(t *testing.T, data []byte) {
	t.Helper()
	var reply struct {
		Error *struct {
			Code    int
			Message string
			Data    struct{ Code, Reason, Remedy string }
		}
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	problem := reply.Error
	if problem.Code != -32700 || problem.Message != "Invalid request arguments." || problem.Data.Code != "invalid_input" || problem.Data.Reason != "Invalid request arguments." || problem.Data.Remedy != "Use the declared fields and limits for this operation." {
		t.Fatal("invalid UTF-8 did not give the complete refusal", problem)
	}
}
