package main

import (
	"encoding/json"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
)

func (a application) resolveRemote(row command, input []byte, options client.Options) (client.Options, error) {
	if err := checkUplinkScope(row, input); err != nil {
		return client.Options{}, err
	}
	return client.Resolve(options, a.base, a.env)
}

func checkUplinkScope(row command, input []byte) error {
	if row.definition.Name != "uplink_add" && row.definition.Name != "uplink_invite" && row.definition.Name != "uplink_scope" {
		return nil
	}
	var args apicontract.UplinkAddArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return err
	}
	_, err := apicontract.ExplicitUplinkScope(args.Scope)
	return err
}
