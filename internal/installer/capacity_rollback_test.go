package installer

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestRunClassChoicesFollowActualRollbackChartContract(t *testing.T) {
	ch, err := loader.Load("testdata/oberth-v0.30.17.tgz")
	if err != nil {
		t.Fatal(err)
	}
	values, err := declaredChartValues(ch, map[string]any{"runs": map[string]any{"parallel": 4, "priorityClasses": map[string]any{"release": 10000}}, "runPolicy": map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(ch, values, "oberth"); err != nil {
		t.Fatal("prior chart rejected later nested choices", err)
	}
	if values["runs"].(map[string]any)["parallel"] != 4 {
		t.Fatal("rollback lost the selected cap")
	}
}
