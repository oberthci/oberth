package main

import (
	"io"
	"testing"
)

func TestServeMemoryHeadroom(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int64
	}{
		{want: 1 << 30},
		{args: []string{"--admission-memory-headroom-bytes=0"}, want: 0},
		{args: []string{"--admission-memory-headroom-bytes=2147483648"}, want: 2 << 30},
	} {
		options, err := parseServeOptions(append([]string{"--argo-namespace=argo"}, tc.args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if *options.declaredAdmission.MemoryHeadroomBytes != tc.want {
			t.Fatalf("headroom flags %v: got %d, %v; want %d", tc.args, *options.declaredAdmission.MemoryHeadroomBytes, err, tc.want)
		}
	}
	for _, value := range []string{"9223372036854775808", "1.5"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-memory-headroom-bytes=" + value}, io.Discard); err == nil {
			t.Fatalf("invalid headroom flag %s accepted", value)
		}
	}
}

func TestServeMemoryHeadroomPercent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int64
	}{
		{want: 10},
		{args: []string{"--admission-memory-headroom-percent=0"}, want: 0},
		{args: []string{"--admission-memory-headroom-percent=25"}, want: 25},
	} {
		options, err := parseServeOptions(append([]string{"--argo-namespace=argo"}, tc.args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if *options.declaredAdmission.MemoryHeadroomPercent != tc.want {
			t.Fatalf("percent = %d, want %d", *options.declaredAdmission.MemoryHeadroomPercent, tc.want)
		}
	}
	for _, value := range []string{"9223372036854775808", "1.5"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-memory-headroom-percent=" + value}, io.Discard); err == nil {
			t.Fatalf("invalid percent %s accepted", value)
		}
	}
}

func TestServeLegacyResourceChargesAreIgnored(t *testing.T) {
	for _, value := range []string{`{"L":{"cpu_milli":4500,"memory_bytes":10737418240}}`, `no-json`, `{"XXL":{"cpu_milli":1000,"memory_bytes":1}}`, `{"L":{"cpu_milli":0}}`, `{"L":{"memory_bytes":1.5}}`} {
		options, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-cpu-millis=4000", "--admission-memory-bytes=8589934592", "--admission-legacy-resources=" + value}, io.Discard)
		if err != nil || options.declaredAdmission.LegacyResources != nil {
			t.Fatalf("retired legacy charges affect startup: %q %+v %v", value, options.declaredAdmission.LegacyResources, err)
		}
	}
}
