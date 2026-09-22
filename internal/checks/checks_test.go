package checks

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsThreeVerdicts(t *testing.T) {
	for _, verdict := range []string{"pass", "warn", "fail"} {
		check, err := Parse("checks/commit-judge.json", []byte(`{"name":"commit-judge","verdict":"`+verdict+`","summary":"describes 0.52"}`))
		if err != nil {
			t.Fatalf("verdict %s: %v", verdict, err)
		}
		if check.Name != "commit-judge" || check.Verdict != verdict || check.Summary != "describes 0.52" {
			t.Fatalf("verdict %s: %+v", verdict, check)
		}
	}
}

func TestParseRefusesWhatCannotBeShown(t *testing.T) {
	cases := map[string][]byte{
		"unknown verdict": []byte(`{"name":"x","verdict":"maybe","summary":"s"}`),
		"no name":         []byte(`{"name":" ","verdict":"pass","summary":"s"}`),
		"no summary":      []byte(`{"name":"x","verdict":"pass","summary":""}`),
		"not json":        []byte(`{`),
		"too large":       append([]byte(`{"name":"x","verdict":"pass","summary":"`), make([]byte, MaxBytes)...),
	}
	for name, body := range cases {
		if _, err := Parse("checks/x.json", body); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		} else if !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v is not ErrRefused", name, err)
		} else if !strings.Contains(err.Error(), "checks/x.json") {
			t.Fatalf("%s: %v does not name the file", name, err)
		}
	}
}

func TestIsCheckNamesOnlyJSONDirectlyUnderTheChecksPrefix(t *testing.T) {
	if !IsCheck("checks/a.json") {
		t.Fatal("checks/a.json is a check")
	}
	for _, name := range []string{"jev/a.json", "checks/a.txt", "checks/nested/a.json", "checks/"} {
		if IsCheck(name) {
			t.Fatalf("%s is not a check", name)
		}
	}
}
