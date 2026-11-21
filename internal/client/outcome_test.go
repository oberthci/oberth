package client

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestUnknownOutcomeGuidanceDoesNotDependOnToolName(t *testing.T) {
	d := definition(t, "secret_put")
	original := uncertain(d, io.ErrUnexpectedEOF)
	d.Name = "another_write"
	renamed := uncertain(d, io.ErrUnexpectedEOF)
	var first, second *publicerr.Error
	if !IsOutcomeUnknown(original) || !IsOutcomeUnknown(renamed) || !errors.As(original, &first) || !errors.As(renamed, &second) {
		t.Fatal("unknown outcome marker missing")
	}
	if first.Remedy() != second.Remedy() {
		t.Fatal("guidance depends on tool name", first.Remedy(), second.Remedy())
	}
}

func TestResponseClassifiesMutationArrivalBeforeRedirectFailure(t *testing.T) {
	for _, name := range []string{"secret_put", "access_allow", "run_get"} {
		d := definition(t, name)
		err := failure("redirect", nil)
		_, absent := exchangeFailure(d, nil, err)
		retry, arrived := exchangeFailure(d, &http.Response{}, err)
		if IsOutcomeUnknown(absent) || IsOutcomeUnknown(arrived) != d.Mutation || d.Mutation && retry {
			t.Fatal("response arrival classification differs", name)
		}
	}
}
