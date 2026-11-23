package main

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestJoinTrustRefusalUsesTheErrorType(t *testing.T) {
	cause := errors.New("trust check failed")
	problem := publicerr.New(publicerr.InvalidInput, "The server pin is missing.", "Choose the server's SSH trust file.", cause)
	problem.ID, problem.Context = "trust-test", []string{"File: chosen_hosts"}
	err := fmt.Errorf("checkout: %w", &serverTrustError{problem})
	refusal := joinTrustRefusal(err)
	var got *publicerr.Error
	if !errors.As(refusal, &got) || got.Remedy() != joinTrustRemedy || got.Code() != problem.Code() || got.Reason() != problem.Reason() || got.ID != problem.ID || !reflect.DeepEqual(got.Context, problem.Context) || !errors.Is(refusal, cause) {
		t.Fatal("typed trust refusal lost its diagnostics or cause", refusal)
	}
	unrelated := publicerr.New(publicerr.Unavailable, "Another operation failed.", serverTrustRemedy, nil)
	for _, original := range []error{unrelated, cause} {
		if got := joinTrustRefusal(original); !errors.Is(got, original) || got.Error() != original.Error() {
			t.Fatal("join changed an unrelated refusal", got)
		}
	}
}

func TestCheckoutTrustMarksMissingAndMismatchedPins(t *testing.T) {
	command := urlCheckoutCommand(t, fake.Bindings(), true)
	endpoint, err := url.Parse("ssh://git@other.oberth.ci:2222")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "absent_hosts"), command.known} {
		err := checkCheckoutTrust(path, endpoint)
		var trust *serverTrustError
		var problem *publicerr.Error
		if !errors.As(err, &trust) || !errors.As(err, &problem) || len(problem.Context) != 1 {
			t.Fatal("trust refusal is not typed or lost its file", err)
		}
		if !errors.As(joinTrustRefusal(err), &problem) || problem.Remedy() != joinTrustRemedy {
			t.Fatal("join did not ask for a new invitation", err)
		}
	}
}
