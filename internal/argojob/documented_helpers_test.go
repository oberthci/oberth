package argojob

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDocumentedHelperNamesMatchGeneratedContainers(t *testing.T) {
	document, err := os.ReadFile("../../docs/native-scheduling.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(document), "Generated helper container names (including Argo's patched executors):")
	if !ok {
		t.Fatal("helper inventory missing")
	}
	table, _, _ = strings.Cut(table, "- **Post-run collect Pod.**")
	var documented []string
	for _, row := range regexp.MustCompile("(?m)^  [|] `([^`]+)` [|]").FindAllStringSubmatch(table, -1) {
		documented = append(documented, row[1])
	}
	workflow := nonrootBuiltFixture(t, false)
	names := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		var patch struct {
			InitContainers []struct {
				Name string `json:"name"`
			} `json:"initContainers"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		}
		if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
			t.Fatal(err)
		}
		for _, c := range patch.InitContainers {
			names[c.Name] = true
		}
		for _, c := range patch.Containers {
			names[c.Name] = true
		}
	}
	var generated []string
	for name := range names {
		generated = append(generated, name)
	}
	slices.Sort(generated)
	slices.Sort(documented)
	if len(generated) == 0 || !reflect.DeepEqual(generated, documented) {
		t.Fatalf("documented = %v; generated = %v", documented, generated)
	}
	if strings.Contains(string(document), "oberth-wait-executor-token") {
		t.Fatal("obsolete helper name remains")
	}
}
