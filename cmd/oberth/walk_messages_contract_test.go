package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestWalkMessagesRoundTripCurrentCLIFormatter(t *testing.T) {
	samples := []string{
		"Use /tmp/config.json to configure the installation and keep the complete message readable in the evidence table.",
		strings.Repeat("x", 80) + " next",
		strings.Repeat("é", 81) + " next",
		"First message.\n\n" + strings.Repeat("word ", 20) + "last",
	}
	var rendered []string
	for _, sample := range samples {
		var out bytes.Buffer
		if err := writeWrapped(&out, sample); err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, out.String())
	}
	input, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "testdata/walk_contract.py", "messages")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("walk message collector: %v\n%s", err, output)
	}
	var collected [][]string
	if err := json.Unmarshal(output, &collected); err != nil || len(collected) != len(samples) {
		t.Fatalf("invalid collected messages: %v\n%s", err, output)
	}
	for index, sample := range samples {
		if want := strings.Split(sample, "\n\n"); !reflect.DeepEqual(collected[index], want) {
			t.Errorf("formatter round trip: want %q, got %q from %q", want, collected[index], rendered[index])
		}
	}
}
