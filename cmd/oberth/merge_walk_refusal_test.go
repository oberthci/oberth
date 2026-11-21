package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMergedRefusalIDSurvivesWireAndStaysOutOfHumanOutput(t *testing.T) {
	problem := publicerr.New(publicerr.Forbidden, "Confirm the forge host key.", "Verify the offered fingerprint.", errors.New("private-cause"))
	problem.ID = forgeTrustID
	problem.Context = []string{"Offered fingerprint: SHA256:example"}
	problem.Detail = "Complete public diagnostic."
	raw, err := json.Marshal(problem)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct{ ID string }
	if err = json.Unmarshal(raw, &wire); err != nil || wire.ID != forgeTrustID {
		t.Fatal("wire identifier lost", err)
	}
	for _, detail := range []bool{false, true} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.failedDetail(problem, detail)
		want := diagnostic.String()
		diagnostic.Reset()
		if err = app.renderRefusal(raw, commonOptions{detail: detail}); err != nil {
			t.Fatal(err)
		}
		if diagnostic.String() != want || strings.Contains(want, forgeTrustID) || strings.Contains(want, "private-cause") {
			t.Fatal("refusal renderer exposed internal identifier or diverged", want)
		}
		if !strings.Contains(want, "Offered fingerprint:\nSHA256:example") {
			t.Fatal("required trust context hidden", want)
		}
		assertRefusalDetail(t, want, problem.Detail, detail)
	}
}

func TestSecretWriteGuidanceWrapsWithoutChangingOriginalError(t *testing.T) {
	cause := errors.New("private-marker")
	original := publicerr.New(publicerr.Unavailable, "Storage outcome is unknown.", "Read the current state.", cause)
	original.ID, original.Context, original.Detail = "uncertain", []string{"Required context."}, "Public detail."
	wrapped := secretWriteGuidance(original)
	var guidance *publicerr.Error
	if !errors.As(wrapped, &guidance) || guidance == original || !errors.Is(wrapped, original) || !errors.Is(wrapped, cause) {
		t.Fatal("guidance did not wrap the original cause")
	}
	if original.Remedy() != "Read the current state." || !strings.Contains(guidance.Remedy(), "oberth audit") || guidance.ID != original.ID || guidance.Detail != original.Detail {
		t.Fatal("guidance mutated or lost public facts")
	}
	guidance.Context[0] = "Changed copy."
	if original.Context[0] != "Required context." {
		t.Fatal("guidance shares mutable context")
	}
}

func TestRefusalIdentifiersUseOneStyle(t *testing.T) {
	for _, id := range []string{forgeTrustID, releaseRedirectID} {
		if strings.Contains(id, "_") || !strings.Contains(id, "-") {
			t.Fatal("refusal identifier style differs", id)
		}
	}
}
