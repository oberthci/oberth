package installer_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func planStatusOrigins(t *testing.T, p *installer.Plan) map[string]string {
	t.Helper()
	var detail struct {
		Values struct {
			Origins map[string]string `json:"statusOrigins"`
		} `json:"values"`
	}
	if err := json.Unmarshal([]byte(strings.Join(p.Detail(), "\n")), &detail); err != nil {
		t.Fatal(err)
	}
	return detail.Values.Origins
}

func TestStatusOriginsAcrossInstallUpgradeAndRollback(t *testing.T) {
	for _, explicitURL := range []string{"", "https://localhost:30443"} {
		t.Run(explicitURL, func(t *testing.T) {
			f := setup(t)
			f.choices.URL = explicitURL
			installed := prepare(t, f)
			before := planStatusOrigins(t, installed)
			wantURL := "default"
			if explicitURL != "" {
				wantURL = "chosen"
			}
			assertStatusOrigins(t, before, map[string]string{"namespace": "default", "server URL": wantURL})
			apply(t, f, installed)
			m := upgradeFixtureAfter(t, &f)
			parallel := 4
			p, err := m.PlanUpgrade(t.Context(), f.release, installer.Choices{Context: "test", Parallel: &parallel}, installer.UpgradeOptions{NoBackup: true})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Plan.Close()
			after := planStatusOrigins(t, p.Plan)
			assertStatusOrigins(t, after, map[string]string{"namespace": "default", "server URL": wantURL, "parallel runs": "chosen"})
			if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
				t.Fatal(err)
			}
			if err = m.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(f.charts.History.Deployed.Values["statusOrigins"])
			var restored map[string]string
			if err = json.Unmarshal(raw, &restored); err != nil || !reflect.DeepEqual(restored, before) {
				t.Fatal("rollback invented choices", restored, before, err)
			}
		})
	}
}

func assertStatusOrigins(t *testing.T, got, want map[string]string) {
	t.Helper()
	for name, origin := range want {
		if got[name] != origin {
			t.Fatal("operator choice provenance differs", name, got[name], origin)
		}
	}
}

func TestLegacyURLOriginIsOnlyChosenWhenExplicit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		f := setup(t)
		f.choices.URL = "https://localhost:30443"
		apply(t, f, prepare(t, f))
		delete(f.charts.History.Deployed.Values, "statusOrigins")
		m := upgradeFixtureAfter(t, &f)
		choices := installer.Choices{Context: "test"}
		want := ""
		if explicit {
			choices.URL, want = f.choices.URL, "chosen"
		}
		p, err := m.PlanUpgrade(t.Context(), f.release, choices, installer.UpgradeOptions{NoBackup: true})
		if err != nil {
			t.Fatal(err)
		}
		assertStatusOrigins(t, planStatusOrigins(t, p.Plan), map[string]string{"server URL": want})
		p.Plan.Close()
	}
}
