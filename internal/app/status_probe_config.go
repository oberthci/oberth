package app

import (
	"errors"
	"time"
)

const (
	vcsProbeTTL       = time.Minute
	vcsProbeTimeout   = 5 * time.Second
	vcsProbeAggregate = 15 * time.Second
)

type StatusProbeConfig struct {
	UpstreamInterval    time.Duration
	ClusterInterval     time.Duration
	SecretStoreInterval time.Duration
	FailureThreshold    int
}

func DefaultStatusProbeConfig() StatusProbeConfig {
	return StatusProbeConfig{UpstreamInterval: vcsProbeTTL, ClusterInterval: time.Minute, SecretStoreInterval: secretStoreProbeTTL, FailureThreshold: 3}
}

func (c StatusProbeConfig) withDefaults() StatusProbeConfig {
	d := DefaultStatusProbeConfig()
	if c.UpstreamInterval == 0 {
		c.UpstreamInterval = d.UpstreamInterval
	}
	if c.ClusterInterval == 0 {
		c.ClusterInterval = d.ClusterInterval
	}
	if c.SecretStoreInterval == 0 {
		c.SecretStoreInterval = d.SecretStoreInterval
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = d.FailureThreshold
	}
	return c
}

func (c StatusProbeConfig) Validate() error {
	for _, interval := range []time.Duration{c.UpstreamInterval, c.ClusterInterval, c.SecretStoreInterval} {
		if interval < 10*time.Second || interval > time.Hour {
			return errors.New("status probe intervals must be between 10s and 1h")
		}
	}
	if c.FailureThreshold < 1 || c.FailureThreshold > 10 {
		return errors.New("status upstream failure threshold must be between 1 and 10")
	}
	return nil
}

func upstreamRetryDelay(interval time.Duration, failures int) time.Duration {
	ceiling := max(interval, 5*time.Minute)
	// Align to observation cycles without shortening the configured interval.
	ceiling = ceiling / interval * interval
	delay := interval
	for failures > 0 && delay < ceiling {
		delay = min(delay*2, ceiling)
		failures--
	}
	return delay
}
