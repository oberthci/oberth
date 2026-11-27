package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestControlResourceChangesAffectOneComponent(t *testing.T) {
	before := rendered(t, nil)
	for _, tc := range []struct{ component, kind, name string }{
		{"server", "Deployment", "oberth"},
		{"controller", "Deployment", installer.ControllerDeployment},
		{"store", "StatefulSet", "oberth-store"},
	} {
		t.Run(tc.component, func(t *testing.T) {
			values, err := installer.ResourceChoices(`{"` + tc.component + `":{"requests":{"ephemeral-storage":"256Mi"},"limits":{"ephemeral-storage":"3Gi"}}}`)
			if err != nil {
				t.Fatal(err)
			}
			after := rendered(t, values)
			for _, component := range [][2]string{{"Deployment", "oberth"}, {"Deployment", installer.ControllerDeployment}, {"StatefulSet", "oberth-store"}} {
				old := jsonOf(t, object(t, before, component[0], component[1]))
				next := jsonOf(t, object(t, after, component[0], component[1]))
				if component[1] == tc.name {
					if old == next || !strings.Contains(next, `"ephemeral-storage":"256Mi"`) {
						t.Fatal("resource choice did not reach the selected Pod", next)
					}
				} else if old != next {
					t.Fatal("resource choice restarted another control", component)
				}
			}
		})
	}
}

func TestNativeResourceChoicesRejectTemplateInputs(t *testing.T) {
	for _, raw := range []string{
		`{"server":{"requests":{"cpu":"{{fail \"x\"}}"}}}`,
		`{"controller":{"extraArgs":["--other"]}}`,
		`{"store":{"limits":{"memory":"-1Gi"}}}`,
		`{"server":{"requests":{"memory":"2Gi"},"limits":{"memory":"1Gi"}}}`,
		`{"other":{"requests":{"cpu":"1"}}}`, `null`, `{}`, `{} {}`,
	} {
		if _, err := installer.ResourceChoices(raw); err == nil {
			t.Fatal("unsafe resource choice accepted", raw)
		}
	}
}

func TestNativeResourceChoicesSurviveInstallerReviewAndUpgrade(t *testing.T) {
	f := setup(t)
	values, err := installer.ResourceChoices(`{"server":{"requests":{"ephemeral-storage":"256Mi"}},"controller":{"requests":{"ephemeral-storage":"128Mi"}},"store":{"requests":{"ephemeral-storage":"64Mi"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	f.choices.Values = values
	plan := prepare(t, f)
	if !strings.Contains(strings.Join(plan.Detail(), "\n"), "ephemeral-storage") {
		t.Fatal("resources missing from installer review")
	}
	if !strings.Contains(strings.Join(plan.Review(), "\n"), "ephemeral-storage") {
		t.Fatal("resource changes missing from compact approval")
	}
	apply(t, f, plan)
	f.choices = installer.Choices{Context: "test"}
	apply(t, f, prepare(t, f))
	last := f.charts.Applies[len(f.charts.Applies)-1]
	for _, tc := range [][2]string{{"Deployment", "oberth"}, {"Deployment", installer.ControllerDeployment}, {"StatefulSet", "oberth-store"}} {
		if !strings.Contains(jsonOf(t, object(t, rendered(t, last.Values), tc[0], tc[1])), "ephemeral-storage") {
			t.Fatal("saved resource choice lost on upgrade", tc)
		}
	}
}

func TestControlResourceTrialCanRestoreAbsentFields(t *testing.T) {
	f := setup(t)
	var err error
	f.choices.Values, err = installer.ResourceChoices(`{"server":{"requests":{"ephemeral-storage":"256Mi"},"limits":{"ephemeral-storage":"3Gi"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f, prepare(t, f))
	clear, err := installer.ResourceChoices(`{"server":{"requests":{"ephemeral-storage":null},"limits":{"ephemeral-storage":null}}}`)
	if err != nil {
		t.Fatal(err)
	}
	f.choices = installer.Choices{Context: "test", Values: clear}
	apply(t, f, prepare(t, f))
	last := f.charts.Applies[len(f.charts.Applies)-1]
	server := object(t, rendered(t, last.Values), "Deployment", "oberth")
	if strings.Contains(jsonOf(t, server), `"ephemeral-storage"`) {
		t.Fatal("trial fields survived rollback", server)
	}
}
