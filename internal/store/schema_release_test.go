package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"sigs.k8s.io/yaml"
)

func TestPackagedChartUsesCurrentStoreSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "oberth", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var defaults struct {
		TargetSchema int `json:"targetSchema"`
		Server       struct {
			Protocol stateport.Protocol `json:"protocol"`
		} `json:"server"`
	}
	if err := yaml.Unmarshal(raw, &defaults); err != nil {
		t.Fatal(err)
	}
	t.Run("schema", func(t *testing.T) {
		if defaults.TargetSchema != Schema {
			t.Fatalf("chart targetSchema=%d; compiled store.Schema=%d", defaults.TargetSchema, Schema)
		}
	})
	t.Run("protocol", func(t *testing.T) {
		if want := stateport.DefaultProtocol(); defaults.Server.Protocol != want {
			t.Fatalf("chart protocol=%+v; fresh store protocol=%+v", defaults.Server.Protocol, want)
		}
	})
}
