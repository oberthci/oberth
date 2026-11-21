package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestInstallationAuditIsVisibleOnlyToUnscopedAdministrator(t *testing.T) {
	t.Parallel()
	p := people(t)
	call, err := p.f.s.PrepareAdminAuditRecord(t.Context(), adminCaller(), apicontract.AdminAuditRecordArgs{Action: "upgrade.completed", Details: map[string]string{"from": "v0.1.0", "to": "v0.2.0", "revision": "2"}})
	if _, err = execute(t.Context(), call, err); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unscoped", "scoped", "repository filter"} {
		t.Run(mode, func(t *testing.T) {
			caller := adminCaller()
			query := apicontract.AuditListArgs{Action: "upgrade.completed", Limit: 10}
			expected := 0
			switch mode {
			case "unscoped":
				expected = 1
			case "scoped":
				caller.Actor.Scope = []string{"origin"}
				p.f.store.Actors[caller.Actor.Fingerprint] = caller.Actor
				defer func() { p.f.store.Actors[admin().Fingerprint] = admin() }()
			case "repository filter":
				query.Repo = repository
			}
			read, err := p.f.s.PrepareAuditList(t.Context(), caller, query)
			page, err := execute(t.Context(), read, err)
			if err != nil || len(page.Records) != expected {
				t.Fatal("installation audit scope differs", mode, page, err)
			}
		})
	}
}
