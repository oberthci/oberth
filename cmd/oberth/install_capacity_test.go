package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestCapacityReviewIsOfflineAndNeverReturnsUnlimited(t *testing.T) {
	r := installationRequest{name: "install capacity", flags: installationFlags("install capacity")}
	var output bytes.Buffer
	if err := reviewCapacity(r, strings.NewReader(`{"currentCap":8,"origin":"chosen"}`), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Insufficient capacity evidence.") || strings.Contains(output.String(), "Proposed trial") {
		t.Fatal(output.String())
	}
	if err := r.flags.Set("print-host-profile", "true"); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := reviewCapacity(r, strings.NewReader("invalid"), &output); err != nil || !strings.Contains(output.String(), "maxPods: 110") {
		t.Fatal(err, output.String())
	}
}

func TestCapacityReviewRejectsUnknownAndTrailingEvidence(t *testing.T) {
	r := installationRequest{name: "install capacity", flags: installationFlags("install capacity")}
	for _, raw := range []string{`{"unknown":1}`, `{} {}`, strings.Repeat(" ", 16<<20+1) + `{}`} {
		if err := reviewCapacity(r, strings.NewReader(raw), &bytes.Buffer{}); err == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
}

func TestCapacityReviewRejectsOversizedTrailingWhitespace(t *testing.T) {
	r := installationRequest{name: "install capacity", flags: installationFlags("install capacity")}
	if err := reviewCapacity(r, strings.NewReader(`{}`+strings.Repeat(" ", 16<<20)), &bytes.Buffer{}); err == nil {
		t.Fatal("oversized trailing whitespace accepted")
	}
}

func TestCapacityReviewCommandNeedsNoInstallationProfile(t *testing.T) {
	a, output, diagnostic := fixture(t, fake.Bindings())
	a.in = strings.NewReader(`{}`)
	if code := a.run(t.Context(), []string{"install", "capacity", "--output", "json"}); code != 0 || !strings.Contains(output.String(), `"advice"`) {
		t.Fatal("offline review opened installation state", code, output.String(), diagnostic.String())
	}
}
