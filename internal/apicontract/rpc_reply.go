package apicontract

import (
	"bytes"
	"encoding/json"
)

// ResultTypeForID checks correlation before interpreting an extensible modern
// result. Core complete results may omit the discriminator under the wire spec.
func ResultTypeForID(data, id json.RawMessage) (string, error) {
	var wire struct {
		JSONRPC string
		ID      json.RawMessage
		Result  map[string]json.RawMessage
		Error   json.RawMessage
	}
	if json.Unmarshal(data, &wire) != nil || wire.JSONRPC != "2.0" || !bytes.Equal(wire.ID, id) {
		return "", ErrReply
	}
	if len(wire.Error) > 0 && !bytes.Equal(wire.Error, []byte("null")) {
		if wire.Result != nil {
			return "", ErrReply
		}
		return "complete", nil
	}
	if wire.Result == nil {
		return "", ErrReply
	}
	if raw, ok := wire.Result["resultType"]; ok {
		var kind string
		if json.Unmarshal(raw, &kind) != nil || kind == "" {
			return "", ErrReply
		}
		return kind, nil
	}
	return "complete", nil
}

// ToolPayload decodes the canonical tools/call reply for request ID 1.
func ToolPayload(data []byte) (json.RawMessage, bool, bool, error) {
	return ToolPayloadForID(data, json.RawMessage("1"))
}

// ToolPayloadForID checks correlation before interpreting a tool reply.
func ToolPayloadForID(data, id json.RawMessage) (json.RawMessage, bool, bool, error) {
	var wire struct {
		ID      json.RawMessage `json:"id"`
		Version string          `json:"jsonrpc"`
		Result  struct {
			Structured json.RawMessage               `json:"structuredContent"`
			Content    []struct{ Type, Text string } `json:"content"`
			IsError    bool                          `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int
			Message string
			Data    json.RawMessage
		} `json:"error"`
	}
	if json.Unmarshal(data, &wire) != nil || !bytes.Equal(wire.ID, id) || wire.Version != "2.0" {
		return nil, false, false, ErrReply
	}
	if wire.Error != nil {
		if len(wire.Error.Data) == 0 {
			return nil, false, false, ErrReply
		}
		return wire.Error.Data, true, wire.Error.Code == -32001, nil
	}
	result := wire.Result.Structured
	if len(result) == 0 && len(wire.Result.Content) > 0 {
		result = json.RawMessage(wire.Result.Content[0].Text)
	}
	if !json.Valid(result) {
		return nil, false, false, ErrReply
	}
	return result, wire.Result.IsError, false, nil
}
