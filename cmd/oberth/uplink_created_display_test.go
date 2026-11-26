package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestUplinkListOmitsUnknownCreationTime(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, format := range []string{"text", "json"} {
			t.Run(format+"/"+map[bool]string{false: "unknown", true: "known"}[known], func(t *testing.T) {
				checkUplinkCreationTime(t, known, format)
			})
		}
	}
}

func checkUplinkCreationTime(t *testing.T, known bool, format string) {
	t.Helper()
	uplink := apicontract.Uplink{Identity: "worker", Fingerprint: "fingerprint"}
	if known {
		uplink.CreatedAt = time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	}
	b := fake.Bindings()
	b.UplinkList.Handler = &fake.Handler[apicontract.UplinkListArgs, apicontract.IdentityPage]{Value: apicontract.IdentityPage{Uplinks: []apicontract.Uplink{uplink}}}
	app, out, diagnostic := fixture(t, b)
	args := []string{"uplink", "list"}
	if format == "json" {
		args = append(args, "--json")
	}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal(code, diagnostic)
	}
	if strings.Contains(out.String(), "0001-01-01") || strings.Contains(out.String(), "2026-11-26T12:00:00Z") != known {
		t.Fatal("creation time is fabricated or lost", out)
	}
	if format == "json" {
		var page struct{ Uplinks []map[string]any }
		if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Uplinks) != 1 {
			t.Fatal("invalid list", err, out)
		}
		if _, present := page.Uplinks[0]["created_at"]; present != known {
			t.Fatal("unknown creation time is present", out)
		}
	} else if strings.Contains(out.String(), "created at:") != known {
		t.Fatal("unknown creation time has a label", out)
	}
}
