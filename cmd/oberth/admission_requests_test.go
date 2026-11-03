package main

import (
	"io"
	"strings"
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
	for _, value := range []string{"-1", "1125899906842625", "9223372036854775808", "1.5"} {
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
	for _, value := range []string{"-1", "101", "9223372036854775808", "1.5"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-memory-headroom-percent=" + value}, io.Discard); err == nil {
			t.Fatalf("invalid percent %s accepted", value)
		}
	}
}

func TestServeLegacyResourceCharges(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace=argo", `--admission-legacy-resources={"L":{"cpu_milli":4500,"memory_bytes":10737418240}}`}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	charge := options.declaredAdmission.LegacyResources["L"]
	if charge.CPUMilli != 4500 || charge.MemoryBytes != 10<<30 {
		t.Fatalf("custom charge lost: %+v", charge)
	}
	for _, value := range []string{`no-json`, `{"XXL":{"cpu_milli":1000,"memory_bytes":1}}`, `{"L":{"cpu_milli":0,"memory_bytes":1}}`, `{"L":{"cpu_milli":1000}}`, `{"L":{"cpu_milli":1.5,"memory_bytes":1}}`} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-legacy-resources=" + value}, io.Discard); err == nil {
			t.Fatalf("invalid legacy charge accepted: %s", value)
		}
	}
}

func TestServeLegacyResourceChargesExceedBudget(t *testing.T) {
	for _, tc := range []struct {
		value, dimension string
	}{
		{`{"L":{"cpu_milli":4001,"memory_bytes":8589934592}}`, "CPU"},
		{`{"L":{"cpu_milli":4000,"memory_bytes":8589934593}}`, "memory"},
	} {
		_, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-cpu-millis=4000", "--admission-memory-bytes=8589934592", "--admission-legacy-resources=" + tc.value}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "L") || !strings.Contains(err.Error(), tc.dimension) || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("startup must name the oversized charge: %v", err)
		}
	}
	for _, dimension := range []struct {
		cpu, memory, name string
	}{
		{"4001", "8589934592", "CPU"},
		{"4000", "8589934593", "memory"},
	} {
		args := []string{"--argo-namespace=argo", "--admission-cpu-millis=4000", "--admission-memory-bytes=8589934592", `--admission-legacy-resources={"XL":{"cpu_milli":` + dimension.cpu + `,"memory_bytes":` + dimension.memory + `}}`}
		if _, err := parseServeOptions(args, io.Discard); err != nil {
			t.Fatalf("exclusive XL configuration must be capped: %v", err)
		}
		if _, err := parseServeOptions(append(args, "--admission-size-weights=XL=4"), io.Discard); err == nil || !strings.Contains(err.Error(), "XL") || !strings.Contains(err.Error(), dimension.name) {
			t.Fatalf("shared XL startup must name the oversized charge: %v", err)
		}
	}
	if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-cpu-millis=4000", "--admission-memory-bytes=8589934592"}, io.Discard); err != nil {
		t.Fatalf("default exclusive XL must allow a small budget: %v", err)
	}
}
