package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
)

func registerStatusProbeFlags(flags *flag.FlagSet, config *app.StatusProbeConfig) {
	d := app.DefaultStatusProbeConfig()
	flags.DurationVar(&config.UpstreamInterval, "status-upstream-probe-interval", d.UpstreamInterval, "background upstream authentication interval (10s-1h; failures back off)")
	flags.DurationVar(&config.ClusterInterval, "status-cluster-probe-interval", d.ClusterInterval, "background Kubernetes diagnostic interval (10s-1h)")
	flags.DurationVar(&config.SecretStoreInterval, "status-secretstore-probe-interval", d.SecretStoreInterval, "background secret-store login diagnostic interval (10s-1h)")
	flags.IntVar(&config.FailureThreshold, "status-upstream-failure-threshold", d.FailureThreshold, "consecutive upstream probe failures before reporting unavailable (1-10)")
}

func upstreamProbeCredentials(options serveOptions, upstream model.Upstream) ([]byte, []byte, error) {
	keyPath := options.upstreamKey
	if upstream.KeyName != "" && app.ValidUpstreamKeyName(upstream.KeyName) {
		candidate := filepath.Join(filepath.Dir(options.upstreamKey), upstream.KeyName)
		if _, err := os.Stat(candidate); err == nil {
			keyPath = candidate
		}
	}
	privateKey, err := readBoundedFile(keyPath, 1<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream identity: %w", err)
	}
	pins, err := readBoundedFile(options.knownHosts, 1<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream host pins: %w", err)
	}
	return privateKey, pins, nil
}

func configureStatusProbes(health *app.Health, options serveOptions) {
	health.StatusProbes = options.statusProbes
	health.PrepareVCS = func(_ context.Context, upstream model.Upstream) (string, func(context.Context) error, error) {
		if upstream.Kind == "local" {
			return "", func(context.Context) error {
				info, err := os.Stat(upstream.BaseURL)
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return errors.New("local upstream is not a directory")
				}
				return nil
			}, nil
		}
		key, pins, err := upstreamProbeCredentials(options, upstream)
		if err != nil {
			return "", nil, err
		}
		// Bind the sampled projected credentials, including fallback and
		// rotation. The digest stays internal and never appears in status.
		identity := fmt.Sprintf("%x:%x", sha256.Sum256(key), sha256.Sum256(pins))
		return identity, func(ctx context.Context) error {
			return app.ProbeSSHAuthentication(ctx, upstream.BaseURL, key, pins)
		}, nil
	}
	health.VCS = func(ctx context.Context, upstream model.Upstream) error {
		_, probe, err := health.PrepareVCS(ctx, upstream)
		if err != nil {
			return err
		}
		return probe(ctx)
	}
}
