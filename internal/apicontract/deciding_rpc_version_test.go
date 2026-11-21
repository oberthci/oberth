package apicontract_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestToolReplyRequiresExactProtocolVersion(t *testing.T) {
	for _, version := range []string{"2.0", "1.0", "", "2.0 "} {
		data, err := json.Marshal(map[string]any{"jsonrpc": version, "id": 1, "result": map[string]any{"structuredContent": map[string]any{"accepted": true}}})
		if err != nil {
			t.Fatal(err)
		}
		payload, refused, retry, err := apicontract.ToolPayload(data)
		if version == "2.0" {
			if err != nil || refused || retry || string(payload) != `{"accepted":true}` {
				t.Fatal("valid reply changed", err)
			}
		} else if !errors.Is(err, apicontract.ErrReply) || payload != nil || refused || retry {
			t.Fatal("wrong protocol accepted", version, err)
		}
	}
}
