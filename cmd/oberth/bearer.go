package main

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
)

type remoteCall struct {
	definition apicontract.Definition
	input      []byte
	bearerFile string
}

func callRemote(ctx context.Context, remote *client.Client, call remoteCall) (json.RawMessage, bool, error) {
	if call.bearerFile == "" {
		return remote.Call(ctx, call.definition, call.input)
	}
	var args apicontract.UplinkAddArgs
	if err := json.Unmarshal(call.input, &args); err != nil {
		return nil, false, err
	}
	if args.BearerDigest != "" {
		return nil, false, usageProblems{"Choose --bearer-file or --bearer-digest, not both."}
	}
	var data json.RawMessage
	var refused bool
	err := client.ActivateBearer(ctx, call.bearerFile, func(ctx context.Context, digest string) error {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(call.input, &object); err != nil {
			return err
		}
		object["bearer_digest"], _ = json.Marshal(digest)
		body, err := json.Marshal(object)
		if err == nil {
			data, refused, err = remote.Call(ctx, call.definition, body)
		}
		return err
	})
	if err != nil || refused {
		return data, refused, err
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, false, err
	}
	out["token_file"], _ = json.Marshal(call.bearerFile)
	data, err = json.Marshal(out)
	return data, false, err
}
