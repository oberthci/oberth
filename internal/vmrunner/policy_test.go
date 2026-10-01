package vmrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type policyReader struct {
	baselineSHA                     string
	candidate, baseline             []byte
	candidateExists, baselineExists bool
	err                             error
}

func (reader policyReader) ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error) {
	return reader.candidate, reader.candidateExists, nil
}

func (reader policyReader) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return reader.baselineSHA, reader.baseline, reader.baselineExists, reader.err
}

func TestResolveRequirementRetainsPublishedSuiteAndRejectsUncertainPolicy(t *testing.T) {
	contract := []byte("version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n")
	for _, test := range []struct {
		name                    string
		reader                  policyReader
		wantRequired, wantError bool
	}{
		{name: "unborn with no requirement"},
		{name: "candidate declaration on unborn upstream", reader: policyReader{candidate: contract, candidateExists: true}, wantRequired: true},
		{name: "deleted published requirement", reader: policyReader{baselineSHA: strings.Repeat("b", 40), baseline: contract, baselineExists: true}, wantRequired: true},
		{name: "missing baseline identity", reader: policyReader{baseline: contract, baselineExists: true}, wantError: true},
		{name: "invalid baseline identity", reader: policyReader{baselineSHA: "main"}, wantError: true},
		{name: "unavailable upstream", reader: policyReader{err: errors.New("offline")}, wantError: true},
		{name: "absent candidate returned content", reader: policyReader{candidate: contract}, wantError: true},
		{name: "absent baseline returned content", reader: policyReader{baseline: contract}, wantError: true},
		{name: "empty existing declaration", reader: policyReader{candidateExists: true}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveRequirement(context.Background(), test.reader, "upstream/org/repo", strings.Repeat("a", 40))
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err == nil && ((got.Contract != nil) != test.wantRequired || got.CandidateSHA != strings.Repeat("a", 40) || got.BaselineSHA != test.reader.baselineSHA) {
				t.Fatalf("requirement = %#v", got)
			}
		})
	}
}
