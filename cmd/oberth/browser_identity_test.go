package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestUplinkAddBearerFileNeedsNoSSHOption(t *testing.T) {
	b := credentialBinding("uplink_add", "")
	handler := b.UplinkAdd.Handler.(*fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity])
	app, _, diagnostic := fixture(t, b)
	path := filepath.Join(filepath.Dir(app.env("OBERTH_TOKEN_FILE")), "browser-bearer")
	if code := app.run(t.Context(), []string{"uplink", "add", "--identity", "browser", "--bearer-file", path}); code != 0 {
		t.Fatal("browser-only CLI refused", code, diagnostic.String())
	}
	args, _, calls := handler.Observed()
	if calls != 1 || args[0].SshPublicKey != "" || len(args[0].BearerDigest) != 64 {
		t.Fatal("browser registration has no private bearer", args, calls)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("browser bearer was not saved privately", info, err)
	}
}
