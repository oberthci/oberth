package service

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestSecretWriterRefusalsAreCompleteAndBounded(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"body", "size", "grant", "write"} {
		err := publicerr.Classify(secretRefusal(code))
		if problem := publicerr.CheckMessage(err.Reason(), 80); problem != nil {
			t.Fatal(code, problem)
		}
		if problem := publicerr.CheckMessage(err.Remedy(), 120); problem != nil {
			t.Fatal(code, problem)
		}
	}
}

func TestSecretDeleteRefusalsAreCompleteAndBounded(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"store", "audit", "configuration"} {
		t.Run(code, func(t *testing.T) {
			err := publicerr.Classify(secretDeleteRefusal(code, nil))
			checkSecretGrantMessage(t, err.Reason(), err.Remedy())
		})
	}
	t.Run("dismiss", func(t *testing.T) {
		reason, remedy := stateport.GrantDismissalRefusal()
		checkSecretGrantMessage(t, reason, remedy)
	})
}

func checkSecretGrantMessage(t *testing.T, reason, remedy string) {
	t.Helper()
	if problem := publicerr.CheckMessage(reason, 40); problem != nil {
		t.Fatal(problem)
	}
	if problem := publicerr.CheckMessage(remedy, 120); problem != nil {
		t.Fatal(problem)
	}
}
