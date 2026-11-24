package client

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

// Invoke decodes a declared tool result, preserving the server's public refusal.
func (c *Client) Invoke(ctx context.Context, name string, input, out any) error {
	d, ok := toolDefinition(name)
	if !ok {
		return failure("reply", nil)
	}
	data, err := json.Marshal(input)
	if err != nil {
		return failure("reply", err)
	}
	result, refused, err := c.Call(ctx, d, data)
	if err != nil {
		return err
	}
	if refused {
		if problem, ok := resultRefusal(result); ok {
			return problem
		}
		return replyFailure(d, nil)
	}
	if out == nil {
		return nil
	}
	if err = json.Unmarshal(result, out); err != nil {
		return replyFailure(d, err)
	}
	return nil
}

//nolint:gochecknoglobals // The client catalogue contains immutable declarations, never service handlers or caller state.
var clientDefinitions = sync.OnceValue(func() map[string]apicontract.Definition {
	out := make(map[string]apicontract.Definition)
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		out[d.Name] = d
	}
	return out
})

func toolDefinition(name string) (apicontract.Definition, bool) {
	d, ok := clientDefinitions()[name]
	return d, ok
}

func resultError(data []byte) error {
	problem, ok := resultRefusal(data)
	if !ok {
		return failure("reply", nil)
	}
	return problem
}

func resultRefusal(data []byte) (*publicerr.Error, bool) {
	var wire struct {
		ID                     string
		Context                []string
		Code                   publicerr.Code
		Reason, Remedy, Detail string
	}
	if json.Unmarshal(data, &wire) != nil || !wire.Code.Valid() || publicerr.CheckMessage(wire.Reason, 80) != nil || publicerr.CheckMessage(wire.Remedy, 120) != nil || wire.Reason == "" || wire.Remedy == "" {
		return nil, false
	}
	err := publicerr.New(wire.Code, wire.Reason, wire.Remedy, nil)
	err.ID = wire.ID
	err.Context = wire.Context
	err.Detail = wire.Detail
	return err, true
}
