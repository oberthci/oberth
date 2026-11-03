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
