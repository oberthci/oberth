package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const administrativeActor = "admin@localhost"
const defaultUpstreamKeySecret = "oberth-upstream-key"       //nolint:gosec // G101: fixed bundle name, not credential material.
const defaultUpstreamKnownHostsSecret = "oberth-known-hosts" //nolint:gosec // G101: fixed bundle name, not credential material.
type gatedAdminTokens struct {
	*store.Store
	Gate   func(context.Context, string) error
	Policy model.UplinkTokenPolicy
}

func (tokens gatedAdminTokens) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.create"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.CreateTokenCredentialWithPolicy(ctx, spec, tokens.Policy)
}

func (tokens gatedAdminTokens) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	if err := tokens.Gate(ctx, "uplink.token.activate"); err != nil {
		return model.TokenCredential{}, err
	}
	return tokens.Store.ActivateTokenCredential(ctx, id)
}

func upstreamAuthStatus(ctx context.Context, upstream model.Upstream, repository, sharedKey, keyDirectory, knownHosts string) string {
	if upstream.Kind != "ssh" {
		return "-"
	}
	keyPath := sharedKey
	if upstream.KeyName != "" && app.ValidUpstreamKeyName(upstream.KeyName) {
		keyPath = filepath.Join(keyDirectory, upstream.KeyName)
	}
	privateKey, err := readBoundedFile(keyPath, 1<<20)
	if err != nil {
		return "error: read upstream identity"
	}
	defer clear(privateKey)
	hosts, err := readBoundedFile(knownHosts, 1<<20)
	if err != nil {
		return "error: read host pins"
	}
	if err := app.ProbeSSHAuthentication(ctx, upstream.BaseURL, privateKey, hosts); err != nil {
		message := err.Error()
		if strings.Contains(message, "unable to authenticate") {
			return "pending: the forge has not accepted this deploy key"
		}
		return "error: " + message
	}
	if repository == "" {
		return "ok"
	}
	if err := app.ProbeSSHRepository(ctx, upstream.BaseURL, repository, privateKey, hosts); err != nil {
		return "error: repository " + repository + ": " + err.Error()
	}
	return "ok (read " + repository + ")"
}

func splitScope(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
