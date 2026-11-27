package main

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

func TestCLIConfirmsLifecycleReceiptsAcrossReleaseBoundary(t *testing.T) {
	for _, transition := range []struct {
		from, to, action string
		rollback         bool
	}{
		{"v0.30.18", "v0.30.19", "upgrade.completed", false},
		{"v0.30.19", "v0.30.18", "rollback.completed", true},
		{"v0.30.19", "v0.30.19", "upgrade.completed", false},
	} {
		t.Run(transition.from+"/"+transition.to, func(t *testing.T) {
			b := fake.Bindings()
			records := &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: "recorded"}}
			b.AdminAuditRecord.Handler = records
			app, _, diagnostic := fixture(t, b)
			native := app.config.Transport
			app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/mcp" {
					return nil, errors.New("MCP versions differ")
				}
				return native.RoundTrip(r)
			})
			options, err := client.Resolve(client.Options{}, app.base, app.env)
			if err != nil {
				t.Fatal(err)
			}
			cfg := app.config
			cfg.Options = options
			remote, err := client.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			s := installationSession{app: app, name: "chosen"}
			s.recordLifecycle(t.Context(), remote, installer.UpgradeCompletion{From: transition.from, To: transition.to, Revision: 3}, transition.rollback)
			args, _, calls := records.Observed()
			want := []apicontract.AdminAuditRecordArgs{{Action: transition.action, Details: map[string]string{"from": transition.from, "to": transition.to, "revision": "3"}}}
			if calls != 1 || !reflect.DeepEqual(args, want) || diagnostic.Len() != 0 {
				t.Fatal("completed lifecycle audit was unconfirmed, changed or repeated", args, calls, diagnostic.String())
			}
		})
	}
}
