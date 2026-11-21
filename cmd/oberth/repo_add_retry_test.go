package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

// The printed retry command repeats the forge address as the person gave it.
func TestRepoAddRetryKeepsTheGivenAddress(t *testing.T) {
	for _, address := range []string{"ssh://git@forge.oberth.ci:33022/team/widget.git", "git@forge.oberth.ci:team/widget.git"} {
		t.Run(address, func(t *testing.T) {
			b := fake.Bindings()
			unconfirmed := publicerr.New(publicerr.Forbidden, forgeTrustReason, "Verify an offered fingerprint, then retry with --expected-host-fingerprint.", nil)
			unconfirmed.ID = forgeTrustID
			b.UpstreamAdd.Handler = &fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]{Error: unconfirmed}
			command := urlCheckoutCommand(t, b, false)
			command.address = address
			if code := command.run(t); code != 1 {
				t.Fatal("unconfirmed host accepted", code, command.diagnostic.String())
			}
			if !strings.Contains(command.diagnostic.String(), "Retry:\noberth repo add "+shellArgument(address)+" --known-hosts ") {
				t.Fatal("retry command changed the given address", command.diagnostic.String())
			}
		})
	}
}
