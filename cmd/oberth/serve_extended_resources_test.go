package main

import (
	"encoding/json"
	"io"
	"testing"
)

func TestServeExtendedResourceBudgetsAreExplicitAndBounded(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace", "pipelines", "--admission-extended-resources", `{"devices.example.test/worker":2}`}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(options.declaredAdmission)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct{ ExtendedResources map[string]int64 }
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ExtendedResources["devices.example.test/worker"] != 2 {
		t.Fatalf("budget not retained: %s", encoded)
	}
	for _, bad := range []string{`{"worker":1}`, `{"devices.example.test/worker":0}`, `{"devices.example.test/worker":-1}`, `{"devices.example.test/worker":0.5}`, `{"devices.example.test/worker":1000001}`, `{"kubernetes.io/worker":1}`} {
		if _, err = parseServeOptions([]string{"--argo-namespace", "pipelines", "--admission-extended-resources", bad}, io.Discard); err == nil {
			t.Errorf("invalid extended resource budget accepted: %s", bad)
		}
	}
}
