//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

func TestInviteCommandNamesItsRealUpstreamScope(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	key := issuerKey(t, advertisedSSH(f.config), f.hostPins(t))
	app.env = func(name string) string {
		switch name {
		case "OBERTH_URL":
			return f.config.Public.URL
		case "OBERTH_CLIENT_KEY":
			return key
		}
		return ""
	}
	app.config.Now = f.deps.clock.Now
	app.config.Transport = f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport
	path := filepath.Join(app.base, "scope.json")
	if code := app.run(t.Context(), []string{"invite", "origin", "--out", path}); code != 0 {
		t.Fatal("upstream invitation refused", code, diagnostic.String())
	}
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err = json.Unmarshal(raw, &invitation); err != nil || len(invitation.Scope) != 1 || invitation.Scope[0] != "origin" {
		t.Fatal("invitation changed upstream scope", err)
	}
	out.Reset()
	diagnostic.Reset()
	if code := app.run(t.Context(), []string{"invite", "origin/team/widget", "--out", filepath.Join(app.base, "refused.json")}); code != 1 {
		t.Fatal("repository scope was accepted or widened", code)
	}
	if !strings.Contains(diagnostic.String(), "upstream name") || !strings.Contains(diagnostic.String(), "origin") {
		t.Fatal("scope refusal hides the upstream form", diagnostic.String())
	}
}
