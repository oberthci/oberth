package client

import (
	"encoding/json"
	"net/http"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type rpcExchange struct {
	request  rpcRequest
	headers  http.Header
	progress func() error
}

func (w rpcExchange) toolRequest(name string, input json.RawMessage) ([]byte, error) {
	params := map[string]json.RawMessage{}
	if len(w.request.Params) > 0 {
		if err := json.Unmarshal(w.request.Params, &params); err != nil {
			return nil, err
		}
	}
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	params["name"], _ = json.Marshal(name)
	params["arguments"] = input
	var err error
	w.request.Params, err = json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(w.request)
}
