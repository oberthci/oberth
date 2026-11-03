package main

import (
	"io"
	"testing"
)

func TestServeIgnoresDeprecatedExtendedAdmissionBudgets(t *testing.T) {
	for _, value := range []string{`{"devices.example.test/worker":2}`, `{}`, `{"worker":-1}`, `retired`} {
		options, err := parseServeOptions([]string{"--argo-namespace=pipelines", "--admission-extended-resources=" + value}, io.Discard)
		if err != nil || options.declaredAdmission.ExtendedResources != nil {
			t.Fatalf("deprecated capacity affected admission: %+v %v", options.declaredAdmission, err)
		}
	}
}
