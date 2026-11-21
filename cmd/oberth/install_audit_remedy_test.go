package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeAuditAdviceKeepsTheSelectedProfileShellQuoted(t *testing.T) {
	for _, scenario := range []struct{ name, command string }{
		{"installed", "OBERTH_PROFILE='installed' oberth audit"},
		{"profile'quoted", "OBERTH_PROFILE='profile'\"'\"'quoted' oberth audit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			remote, err := client.New(client.Config{
				Options: client.Options{URL: "https://oberth.ci"}, Transport: remedyLostReply{},
				Now:  func() time.Time { return time.Unix(0, 0) },
				Wait: func(context.Context, time.Duration) error { return errors.New("unexpected retry") },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			var diagnostic bytes.Buffer
			session := installationSession{app: application{diagnostic: &diagnostic}, name: scenario.name}
			session.recordLifecycle(t.Context(), remote, installer.UpgradeCompletion{}, false)
			want := "Upgrade succeeded; audit unconfirmed. Check: " + scenario.command + "\n"
			if diagnostic.String() != want {
				t.Fatal("upgrade advice must use the supported profile environment", diagnostic.String())
			}
		})
	}
}
