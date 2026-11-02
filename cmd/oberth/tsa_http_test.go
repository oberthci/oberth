package main

import (
	"io"
	"testing"
)

func TestServeAllowsTSAHTTPOnlyWithOptIn(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		args := []string{"--argo-namespace", "argo-pipelines", "--audit-tsa-url", "http://tsa.example.test"}
		if insecure {
			args = append(args, "--audit-tsa-insecure-http")
		}
		_, err := parseServeOptions(args, io.Discard)
		if (err == nil) != insecure {
			t.Errorf("insecure=%t: %v", insecure, err)
		}
	}
}
