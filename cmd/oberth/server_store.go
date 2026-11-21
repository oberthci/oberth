package main

import (
	"context"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"net/url"
)

// The admission token projection and both store login roles share this value.
func storeConfiguration(c serverConfig) secretstore.Config {
	return secretstore.Config{
		Mount: c.SecretStore.KVMount, AuthMount: c.SecretStore.AuthMount,
		WriterRole: c.SecretStore.WriterRole, StepRole: "oberth-step",
		ServerNamespace: c.Namespace, PipelineNamespace: c.PipelineNamespace,
		ServerAccount: "oberth", Audience: c.SecretStore.Audience,
	}
}

func serverStoreWriter(ctx context.Context, c serverConfig) (secretstore.Writer, runtimeconfig.SecretDelivery, error) {
	address, err := url.Parse(c.SecretStore.Address)
	if err != nil {
		return secretstore.Writer{}, runtimeconfig.SecretDelivery{}, serverProblem("config", "secret_store.address: invalid URL", err)
	}
	return secretstore.ServerWriter(ctx, secretstore.Connection{URL: c.SecretStore.Address, ServerName: address.Hostname()}, storeConfiguration(c), secretstore.ServerFiles{SecretStoreCA: c.Files.SecretStoreCA, SecretStoreToken: c.Files.SecretStoreToken})
}
