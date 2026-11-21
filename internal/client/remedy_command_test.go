package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestUnknownMutationOutcomeNamesTheHumanCheckCommand(t *testing.T) {
	d := definition(t, "admin_audit_record")
	err := uncertain(d, errors.New("injected lost reply"))
	var problem *publicerr.Error
	if !errors.As(err, &problem) {
		t.Fatal(err)
	}
	want := "Check with oberth audit before submitting it again."
	if problem.Remedy() != want || strings.Contains(err.Error(), "audit list") {
		t.Fatal("check advice must use the declaration's human command", err)
	}
}

func TestUnknownCheckKeepsGenericRecoveryAdvice(t *testing.T) {
	d := definition(t, "admin_audit_record")
	for _, name := range []string{"missing-check", "admin_audit_record"} {
		d.Check = name
		err := uncertain(d, errors.New("injected lost reply"))
		if strings.Contains(err.Error(), "oberth ") {
			t.Fatal("unknown or raw-only checks need generic advice", err)
		}
	}
}
