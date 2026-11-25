package main

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
)

type secretStepInputs struct {
	connection     secretstore.Connection
	paths          []string
	token, scratch string
}

func (a application) secretStepEnvironment() (secretStepInputs, error) {
	var inputs secretStepInputs
	if a.env == nil {
		return inputs, stepEnvironmentProblem(runtimeconfig.SecretConfigEnv)
	}
	raw := a.env(runtimeconfig.SecretConfigEnv)
	var delivery runtimeconfig.SecretDelivery
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 1<<20 || !json.Valid([]byte(raw)) || d.Decode(&delivery) != nil {
		return inputs, stepEnvironmentProblem(runtimeconfig.SecretConfigEnv)
	}
	connection, err := stepBrokerConnection(delivery)
	if err != nil {
		return inputs, err
	}
	inputs.connection = connection
	raw = a.env(runtimeconfig.SecretPathsEnv)
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &inputs.paths) != nil || len(inputs.paths) == 0 {
		return inputs, stepEnvironmentProblem(runtimeconfig.SecretPathsEnv)
	}
	inputs.token, inputs.scratch = a.env(runtimeconfig.SecretTokenEnv), a.env(runtimeconfig.SecretScratchEnv)
	for name, path := range map[string]string{runtimeconfig.SecretTokenEnv: inputs.token, runtimeconfig.SecretScratchEnv: inputs.scratch} {
		if !filepath.IsAbs(path) {
			return inputs, stepEnvironmentProblem(name)
		}
	}
	return inputs, nil
}

func stepBrokerConnection(delivery runtimeconfig.SecretDelivery) (secretstore.Connection, error) {
	if secretstore.Config(delivery.Access).Validate() != nil || !runtimeconfig.ValidStoreAddress(delivery.BrokerAddress) || delivery.BrokerServerName == "" || delivery.BrokerCAPEM == "" || len(delivery.BrokerCAPEM) > runtimeconfig.SecretCABytes {
		return secretstore.Connection{}, stepEnvironmentProblem(runtimeconfig.SecretConfigEnv)
	}
	return secretstore.Connection{URL: delivery.BrokerAddress, ServerName: delivery.BrokerServerName, CA: []byte(delivery.BrokerCAPEM)}, nil
}
func stepEnvironmentProblem(name string) error {
	problem := installationProblem("Step secret-store inputs are missing or invalid.", "Run this command in a step with an approved secret grant.")
	problem.Context = []string{"Input: " + name}
	return problem
}
