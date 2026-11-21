package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestOwnMissingDefinitionsUseChartRepair(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"one", "all"} {
		t.Run(missing, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "oberth")
			apply(t, f, prepare(t, f))
			if missing == "all" {
				clear(reader.Objects)
			} else {
				delete(reader.Objects, "workflows.argoproj.io")
			}
			prepare(t, f)
			files, err := installer.Render(installer.ReleaseChartForTest(f.release), f.charts.Previews[len(f.charts.Previews)-1].Values, "oberth")
			if err != nil || strings.Count(jsonOf(t, files), "kind: CustomResourceDefinition") != 8 {
				t.Fatal("own repair missing from chart", err)
			}
		})
	}
}

func TestOwnMissingDefinitionAmongBorrowedDefinitionsUsesChartRepair(t *testing.T) {
	t.Parallel()
	f := setup(t)
	reader := definitionFixture(t, f, "control")
	reader.Objects["workflows.argoproj.io"].Annotations["meta.helm.sh/release-namespace"] = "oberth"
	apply(t, f, prepare(t, f))
	delete(reader.Objects, "workflows.argoproj.io")
	plan := prepare(t, f)
	files, err := installer.Render(installer.ReleaseChartForTest(f.release), f.charts.Previews[len(f.charts.Previews)-1].Values, "oberth")
	if err != nil || strings.Count(jsonOf(t, files), "kind: CustomResourceDefinition") != 1 || !strings.Contains(jsonOf(t, files), "name: workflows.argoproj.io") {
		t.Fatal("own missing definition not repaired", err)
	}
	apply(t, f, plan)
}

func TestMissingBorrowedDefinitionsStillRequireTheirOwner(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"one", "all"} {
		t.Run(missing, func(t *testing.T) {
			f := setup(t)
			reader := definitionFixture(t, f, "control")
			apply(t, f, prepare(t, f))
			if missing == "all" {
				clear(reader.Objects)
			} else {
				delete(reader.Objects, "workflows.argoproj.io")
			}
			before := installationRecord(t, f)
			applies := len(f.charts.Applies)
			f.client.ClearActions()
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if err == nil || !strings.Contains(err.Error(), "with their owner") || !strings.Contains(err.Error(), "workflows.argoproj.io") {
				t.Fatal(err)
			}
			if installationRecord(t, f) != before || len(f.charts.Applies) != applies {
				t.Fatal("refusal changed installation")
			}
			for _, action := range f.client.Actions() {
				if !readOnly(action) {
					t.Fatal("refusal wrote", action)
				}
			}
		})
	}
}
