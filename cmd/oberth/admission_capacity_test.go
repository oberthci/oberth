package main

import (
	"io"
	"testing"
)

func TestAdmissionOvercommitOptions(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{{"", 1}, {"1", 1}, {"1.5", 1.5}, {"2", 2}} {
		args := []string{"--argo-namespace=argo"}
		if tc.value != "" {
			args = append(args, "--admission-overcommit-factor="+tc.value)
		}
		options, err := parseServeOptions(args, io.Discard)
		if err != nil || options.admissionOvercommitFactor != tc.want {
			t.Fatalf("factor %q: %v %+v", tc.value, err, options.admissionOvercommitFactor)
		}
	}
	for _, bad := range []string{"0", "0.9", "-1", "NaN", "+Inf"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-overcommit-factor=" + bad}, io.Discard); err == nil {
			t.Fatalf("accepted factor %s", bad)
		}
	}
}
