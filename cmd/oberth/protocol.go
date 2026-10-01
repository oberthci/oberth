package main

import (
	"context"
	"fmt"
	"os"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/store"
)

type protocolContextKey struct{}
type commandProtocol struct {
	store          store.ProtocolConfig
	witnessKeyInfo string
}

// withCommandProtocol reads administrator-owned process configuration exactly
// once, before any command can inspect or mutate database state. Administrative
// commands executed in the same Pod inherit the daemon's identical parameters.
func withCommandProtocol(ctx context.Context) (context.Context, error) {
	p := commandProtocol{store: store.DefaultProtocolConfig(), witnessKeyInfo: auditanchor.DefaultWitnessKeyInfo}
	for _, setting := range []struct {
		name        string
		destination *string
	}{
		{"OBERTH_SCHEMA_IDENTITY", &p.store.SchemaIdentity},
		{"OBERTH_AUDIT_DOMAIN", &p.store.AuditDomain},
		{"OBERTH_WITNESS_KEY_INFO", &p.witnessKeyInfo},
	} {
		if value, present := os.LookupEnv(setting.name); present {
			*setting.destination = value
		}
	}
	if err := p.store.Validate(); err != nil {
		return nil, fmt.Errorf("deployment protocol: %w", err)
	}
	if err := auditanchor.ValidateWitnessKeyInfo(p.witnessKeyInfo); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, protocolContextKey{}, p), nil
}

func configuredStoreOptions(ctx context.Context) store.Options {
	if p, ok := ctx.Value(protocolContextKey{}).(commandProtocol); ok {
		return store.Options{Protocol: p.store}
	}
	return store.Options{}
}
func configuredWitnessKeyInfo(ctx context.Context) string {
	if p, ok := ctx.Value(protocolContextKey{}).(commandProtocol); ok {
		return p.witnessKeyInfo
	}
	return auditanchor.DefaultWitnessKeyInfo
}
