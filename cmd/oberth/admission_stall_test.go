package main

import (
	"io"
	"testing"
	"time"
)

func TestAdmissionStallTimeoutFlag(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", 10 * time.Minute}, {"3m", 3 * time.Minute}} {
		args := []string{"--argo-namespace", "argo-pipelines"}
		if tc.value != "" {
			args = append(args, "--admission-stall-timeout", tc.value)
		}
		options, err := parseServeOptions(args, io.Discard)
		if err != nil || options.admissionStallTimeout != tc.want {
			t.Fatalf("stall timeout = %s, %v; want %s", options.admissionStallTimeout, err, tc.want)
		}

	}
	for _, value := range []string{"0", "-1m"} {
		if _, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-stall-timeout", value}, io.Discard); err == nil {
			t.Fatalf("accepted unsafe timeout %q", value)
		}
	}
}
