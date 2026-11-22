package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

// checkUpstreamForgeUser keeps a forge's own SSH user and namespace in the
// binding and refuses a user or path that could reach another account.
func checkUpstreamForgeUser(t *testing.T, f AdministrationFixture) {
	a := administrativeAction("upstream-forge-user")
	u := stateport.Upstream{Name: "plain", Org: "repos", Kind: "ssh", BaseURL: "ssh://oberth@forge.oberth.ci/home/oberth/repos", KeyName: "shared"}
	mustRun(t, f.Port.RegisterUpstreamBinding(t.Context(), a, u))
	rows, err := f.Port.UpstreamBindings(t.Context())
	mustRun(t, err)
	found := false
	for _, row := range rows {
		found = found || row == u
	}
	if !found {
		t.Fatal("forge user or namespace was not kept", rows)
	}
	for i, base := range []string{"ssh://-oProxyCommand=x@forge.oberth.ci/team", "ssh://git:secret@forge.oberth.ci/team", "ssh://a%0AUser%20b@forge.oberth.ci/team", "ssh://forge.oberth.ci/home/../team", "https://glpat-AbCdEf0123456789xyz@forge.oberth.ci/team", "https://oauth2@forge.oberth.ci/team", "https://user:token@forge.oberth.ci/team"} {
		refused := stateport.Upstream{Name: "refused-" + string(rune('a'+i)), Org: "refused-" + string(rune('a'+i)), Kind: "ssh", BaseURL: base, KeyName: "shared"}
		if f.Port.RegisterUpstreamBinding(t.Context(), a, refused) == nil {
			t.Fatal("unsafe forge address stored", base)
		}
	}
}
