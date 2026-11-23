package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeAnnouncesWaitingBeforeProbingOnce(t *testing.T) {
	f := signedMaintenanceFixture(t)
	plan := planSignedFixture(t, &f)
	flags := installationFlags("upgrade")
	if err := flags.Parse([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	f.session.request = installationRequest{name: "upgrade", flags: flags}
	var out bytes.Buffer
	checks := 0
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		if strings.Count(out.String(), "waiting for server (up to 600 s)\n") != 1 {
			t.Fatal("waiting was not printed exactly once before probing", out.String())
		}
		checks++
		if checks < 3 {
			return installer.NewProblem("starting", nil, "")
		}
		return installer.NewProblem("tls", nil, "")
	}
	f.session.engine.Pause = func(context.Context) error { return nil }
	err := f.session.applyUpgrade(t.Context(), plan, upgradeApplication{
		maintenance: installer.Maintenance{Engine: f.session.engine, Options: f.session.options},
		in:          strings.NewReader(""), out: &out,
	})
	if err == nil || checks != 3 || strings.Count(out.String(), "waiting for server") != 1 {
		t.Fatal("upgrade did not keep one waiting line across attempts", checks, err, out.String())
	}
}
