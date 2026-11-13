package main

import (
	"io"
	"strings"
	"testing"
)

func TestRemovedSchedulingFlagsAreErrors(t *testing.T) {
	for _, flag := range []string{
		"admission-disk-floor-bytes=1", "admission-cpu-enabled=true", "admission-cpu-headroom-percent=20", "step-cpu-request-percent=25",
		"admission-pressure-enabled=false", "admission-pressure-hard-cpu=50", "admission-pressure-hard-io=40", "admission-pressure-defer-seconds=300",
	} {
		t.Run(flag, func(t *testing.T) {
			_, err := parseServeOptions([]string{"--" + flag}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("removed flag did not explain refusal: %v", err)
			}
		})
	}
}

func TestRemovedExtendedResourceGrantFlagIsAnExplicitError(t *testing.T) {
	_, err := parseServeOptions([]string{"--admission-extended-resources=example.com/gpu=1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || !strings.Contains(err.Error(), "admission-extended-resources") {
		t.Fatal("retired grant flag was not clearly refused", err)
	}
}
