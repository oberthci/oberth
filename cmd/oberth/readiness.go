package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
)

// upstreamReadinessProbe uses the rows already read by the sampler; it never
// acquires a second DB connection or starts a second upstream snapshot.
func upstreamReadinessProbe(options serveOptions, secretCheck func(context.Context, []model.Upstream) error) func(context.Context, []model.Upstream) error {
	return func(ctx context.Context, registered []model.Upstream) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The Git transport is configured when at least one complete SSH
		// identity validates: the shared fallback key, or any per-upstream
		// key projected from the Secret volume. A fresh install whose first
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
		// File-based checks exhausted. Fall back to the Kubernetes API:
		// after `upstream add` stores an SSH key in the Secret, the kubelet
		// may take up to ~60s to project the update into the volume mount.
		// Reading the Secrets directly confirms configuration without
		// waiting for file projection.
		if secretCheck != nil {
			err := secretCheck(ctx, registered)
			if err == nil {
				return nil
			}
			// The Secret reader has its own shorter deadline. Preserve it even
			// when the outer sampler context still has time remaining.
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return globalErr
	}
}
