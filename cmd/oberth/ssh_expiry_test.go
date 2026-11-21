package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
)

func TestGitSSHExpiredProfileExplainsRenewAndRejoin(t *testing.T) {
	app, _, _ := fixture(t, fake.Bindings())
	identity, _ := composeSSHKey(t, t.TempDir(), client.UplinkPushKeyFile)
	cert, _ := authorityLifetime(t, app.config.Now().Add(-time.Hour), time.Minute)
	if err := os.WriteFile(filepath.Join(filepath.Dir(identity), "admin.crt"), cert, 0600); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []bool{true, false} {
		if !configured {
			app.config.Now = nil
		}
		err := app.streamGitSSH(t.Context(), gitssh.Config{Identity: identity})
		if err == nil || !strings.Contains(err.Error(), "Client certificate has expired") || !strings.Contains(err.Error(), "oberth renew") || !strings.Contains(err.Error(), "join again") {
			t.Fatal("SSH helper hid known credential expiry", configured, err)
		}
	}
}
