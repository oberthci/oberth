package installer_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestRunPrioritiesAreExplicitNonpreemptingAndRetained(t *testing.T) {
	assertRunClassCount(t, rendered(t, nil), 0)
	values, err := installer.RunPriorityChoices(`{"release":10000,"ordinary":0}`)
	if err != nil {
		t.Fatal(err)
	}
	objects := rendered(t, values)
	for _, name := range []string{"release", "ordinary"} {
		class := object(t, objects, "PriorityClass", "oberth-run-oberth-"+name)
		if class["value"] != map[string]float64{"release": 10000, "ordinary": 0}[name] {
			t.Fatal("run class lost its explicitly selected priority", class)
		}
		if class["globalDefault"] != false || class["preemptionPolicy"] != "Never" || class["metadata"].(map[string]any)["annotations"].(map[string]any)["helm.sh/resource-policy"] != "keep" {
			t.Fatal("run class changes preemption or disappears on rollback", class)
		}
	}
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	values["runs"].(map[string]any)["priorityClasses"] = map[string]any{"escape": 1000000}
	if _, err := installer.Render(ch, values, "oberth"); err == nil {
		t.Fatal("run priority reached control class")
	}
}

func TestRunPrioritiesCanBeClearedWithoutChangingCap(t *testing.T) {
	f := setup(t)
	parallel := 7
	f.choices.Parallel = &parallel
	var err error
	f.choices.Values, err = installer.RunPriorityChoices(`{"release":10000}`)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f, prepare(t, f))
	clear, err := installer.RunPriorityChoices(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	f.choices = installer.Choices{Context: "test", Values: clear}
	apply(t, f, prepare(t, f))
	last := f.charts.Applies[len(f.charts.Applies)-1]
	runs := last.Values["runs"].(map[string]any)
	if len(runs["priorityClasses"].(map[string]any)) != 0 || jsonOf(t, runs["parallel"]) != "7" {
		t.Fatal("selected run classes were not cleared or cap changed", runs)
	}
}
