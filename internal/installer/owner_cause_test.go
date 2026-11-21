package installer_test

import (
	"bytes"
	"errors"
	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"
)

func TestOwnerRefusalNamesMissingOrRetainedAdministrator(t *testing.T) {
	for _, cause := range []string{"missing", "retained"} {
		for _, original := range []bool{false, true} {
			t.Run(cause+"/"+boolText(original), func(t *testing.T) { ownerCause(t, cause, original) })
		}
	}
}
func ownerCause(t *testing.T, cause string, original bool) {
	t.Helper()
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	if cause == "missing" {
		if err := f.client.CoreV1().Secrets("oberth").Delete(t.Context(), "oberth-admin", meta.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	} else {
		f.charts.History = installer.History{}
	}
	if !original {
		replaceOwnerCredentials(t, f, installed.Profile, "absent")
	}
	before, applies := installationSnapshot(t, f), len(f.charts.Applies)
	f.client.ClearActions()
	plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if cause == "retained" && original {
		if err != nil {
			t.Fatal("original profile cannot use retained administrator", err)
		}
		plan.Close()
	} else {
		assertOwnerCause(t, cause, err)
	}
	deltaAssertOnlyClusterReads(t, f, 0)
	if !bytes.Equal(before, installationSnapshot(t, f)) || len(f.charts.Applies) != applies {
		t.Fatal("ownership check wrote installation")
	}
}

func assertOwnerCause(t *testing.T, cause string, err error) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "administrator-"+cause {
		t.Fatal("wrong ownership cause", err)
	}
	if !strings.Contains(refusal.Detail, "Secret: oberth/oberth-admin") || !strings.Contains(refusal.Detail, cause) || strings.Contains(refusal.Reason, "does not own") {
		t.Fatal("Secret cause not named", err)
	}
	if len([]rune(refusal.Reason)) > 80 || len([]rune(refusal.Remedy)) > 120 {
		t.Fatal("refusal exceeds budget")
	}
}
