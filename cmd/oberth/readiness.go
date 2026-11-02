package main

import (
	"context"
	"path/filepath"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
)

// upstreamReadinessProbe uses the rows already read by the sampler; it never
// acquires a second DB connection or starts a second upstream snapshot.
func upstreamReadinessProbe(options serveOptions) func(context.Context, []model.Upstream) error {
	return func(ctx context.Context, registered []model.Upstream) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The Git transport is configured when at least one complete SSH
		// identity validates: the shared fallback key, or any per-upstream
		// key materialized from OpenBao. A fresh install whose first
		// `upstream add` provisioned only a dedicated key has no shared key
		// and must still become ready; a broken individual upstream surfaces
		// through the per-upstream VCS probe instead.
		_, globalErr := app.GitSSHCommand(options.upstreamKey, options.knownHosts)
		if globalErr == nil {
			return nil
		}
		keyDirectory := filepath.Dir(options.upstreamKey)
		for _, upstream := range registered {
			if upstream.Kind != "ssh" || upstream.KeyName == "" || !app.ValidUpstreamKeyName(upstream.KeyName) {
				continue
			}
			if _, err := app.GitSSHCommand(filepath.Join(keyDirectory, upstream.KeyName), options.knownHosts); err == nil {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return globalErr
	}
}
