package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Assisted continuation is off by default and every knob is bounded.
func TestServeAssistDefaultOffAndValidated(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || defaults.assist.Enabled || defaults.assist.HoldTTL != 20*time.Minute || defaults.assist.MaxConcurrentHolds != 2 || defaults.assist.ScratchBytes != 8<<30 {
		t.Fatalf("assist defaults = %+v, %v", defaults.assist, err)
	}
	configured, err := parseServeOptions([]string{"--argo-namespace=argo", "--assist-enabled", "--assist-hold-ttl=45m", "--assist-max-concurrent-holds=4", "--assist-exec-timeout=2m", "--assist-exec-output-bytes=131072", "--assist-patch-max-bytes=2097152", "--assist-patch-max-files=16"}, io.Discard)
	if err != nil || !configured.assist.Enabled || configured.assist.HoldTTL != 45*time.Minute || configured.assist.MaxConcurrentHolds != 4 || configured.assist.ExecTimeout != 2*time.Minute || configured.assist.ExecOutputBytes != 131072 || configured.assist.PatchMaxBytes != 2097152 || configured.assist.PatchMaxFiles != 16 {
		t.Fatalf("assist flags not parsed: %+v, %v", configured.assist, err)
	}
	for _, value := range []string{"--assist-hold-ttl=5h", "--assist-hold-ttl=1500ms", "--assist-scratch-bytes=-2", "--assist-scratch-bytes=68719476737", "--assist-max-concurrent-holds=65", "--assist-exec-timeout=16m", "--assist-exec-output-bytes=5000000", "--assist-patch-max-bytes=20000000", "--assist-patch-max-files=2000"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", value}, io.Discard); err == nil {
			t.Fatal("out-of-range assist flag accepted", value)
		}
	}
	if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--assist-enabled", "--assist-hold-ttl=0s"}, io.Discard); err == nil {
		t.Fatal("enabled assist with a zero hold TTL accepted")
	}
}

func TestChartRendersAssistFlagsWithBinaryDefaults(t *testing.T) {
	render := func(sets ...string) (string, error) {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + chartTestDigest}
		for _, set := range sets {
			args = append(args, "--set", set)
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		return string(output), err
	}
	text, err := render()
	if err != nil {
		t.Fatalf("default chart: %v %s", err, text)
	}
	for _, want := range []string{"- --assist-enabled=false", `"--assist-hold-ttl=20m"`, "- --assist-max-concurrent-holds=2", `"--assist-exec-timeout=1m"`, "- --assist-exec-output-bytes=65536", "- --assist-patch-max-bytes=1048576", "- --assist-patch-max-files=64", "- --assist-scratch-bytes=8589934592"} {
		if !strings.Contains(text, want) {
			t.Fatalf("default chart misses %q:\n%s", want, text)
		}
	}
	text, err = render("assist.enabled=true", "assist.holdTTL=30m", "assist.maxConcurrentHolds=3")
	if err != nil || !strings.Contains(text, "- --assist-enabled=true") || !strings.Contains(text, `"--assist-hold-ttl=30m"`) || !strings.Contains(text, "- --assist-max-concurrent-holds=3") {
		t.Fatalf("assist values not rendered: %v %s", err, text)
	}
	if text, err := render("assist.scratchBytes=-1"); err != nil || !strings.Contains(text, "- --assist-scratch-bytes=-1") {
		t.Fatalf("assist.scratchBytes not rendered: %v %s", err, text)
	}
	for _, set := range []string{"assist.maxConcurrentHolds=0", "assist.maxConcurrentHolds=65", "assist.holdTTL=soon", "assist.patchMaxFiles=0", "assist.bogus=1", "assist.scratchBytes=-2", "assist.scratchBytes=68719476737"} {
		if _, err := render(set); err == nil {
			t.Fatal("chart schema accepted", set)
		}
	}
}

// The assist duration values take Go durations, as their serve flags do:
// compound forms such as 1h30m pass the schema; holdTTL must stay whole
// seconds (the server refuses 1500ms), execTimeout may be any duration.
// Every accepted value parses as a flag.
func TestChartAssistDurationValuesAreGoDurations(t *testing.T) {
	render := func(set string) (string, error) {
		output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+chartTestDigest, "--set", set).CombinedOutput()
		return string(output), err
	}
	for set, flag := range map[string]string{
		"assist.holdTTL=1h30m": "--assist-hold-ttl=1h30m", "assist.holdTTL=90s": "--assist-hold-ttl=90s", "assist.holdTTL=2h0m30s": "--assist-hold-ttl=2h0m30s",
		"assist.execTimeout=1m30s": "--assist-exec-timeout=1m30s", "assist.execTimeout=1500ms": "--assist-exec-timeout=1500ms",
	} {
		text, err := render(set)
		if err != nil || !strings.Contains(text, `"`+flag+`"`) {
			t.Fatalf("chart refused or did not render %s: %v %s", set, err, text)
		}
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", flag}, io.Discard); err != nil {
			t.Fatalf("chart accepts %s but the server refuses it: %v", set, err)
		}
	}
	for _, set := range []string{"assist.holdTTL=1500ms", "assist.holdTTL=1.5h", "assist.holdTTL=20 minutes", "assist.holdTTL=m", "assist.execTimeout=soon", "assist.execTimeout=1m-"} {
		if _, err := render(set); err == nil {
			t.Fatal("chart schema accepted", set)
		}
	}
}
