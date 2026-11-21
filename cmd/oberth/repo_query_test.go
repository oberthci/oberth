package main

import (
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoAddQueryAndFragmentRefusedBeforeCheckoutAndDiscovery(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, suffix := range []string{"?access_token=private-marker", "#private-marker", "?", "#", "?#"} {
			t.Run(strconv.FormatBool(joined)+suffix, func(t *testing.T) {
				b := fake.Bindings()
				command := urlCheckoutCommand(t, b, joined)
				command.address += suffix
				command.app.directory = filepath.Join(t.TempDir(), "absent-checkout")
				if code := command.run(t, "--detail"); code != 1 || !strings.Contains(command.diagnostic.String(), "query or fragment") {
					t.Fatal(code, command.diagnostic.String())
				}
				if strings.Contains(command.out.String()+command.diagnostic.String(), "private-marker") {
					t.Fatal("query or fragment was echoed")
				}
				assertNoRepoAdministration(t, b)
				_, _, lists := b.RepoList.Handler.(*fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]).Observed()
				if lists != 0 {
					t.Fatal("query reached discovery")
				}
				if _, err := os.Stat(command.app.directory); !os.IsNotExist(err) {
					t.Fatal("refusal touched checkout", err)
				}
			})
		}
	}
}
