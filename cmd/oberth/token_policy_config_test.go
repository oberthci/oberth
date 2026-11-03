package main

import (
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestServerAcceptsUplinkTokenLifetimeConfiguration(t *testing.T) {
	if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--uplink-token-default-ttl=720h", "--uplink-token-max-ttl=1440h"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestServerValidatesUplinkTokenLifetimeBounds(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.uplinkTokenPolicy.DefaultTTL != 90*24*time.Hour || options.uplinkTokenPolicy.MaxTTL != model.MaximumUplinkTokenTTL {
		t.Fatalf("default policy: %+v", options.uplinkTokenPolicy)
	}
	for _, args := range [][]string{{"--uplink-token-default-ttl=0"}, {"--uplink-token-default-ttl=-1h"}, {"--uplink-token-max-ttl=0"}, {"--uplink-token-max-ttl=8784h"}, {"--uplink-token-max-ttl=1h"}} {
		if _, err := parseServeOptions(append([]string{"--argo-namespace=argo"}, args...), io.Discard); err == nil {
			t.Fatalf("invalid policy accepted: %v", args)
		}
	}
}
