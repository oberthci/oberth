package api

import (
	"fmt"
	"testing"
)

func TestFourPriorityClassSchemas(t *testing.T) {
	for _, tool := range toolDefinitions() {
		schema := tool["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		value, ok := properties["priority"].(map[string]any)
		if !ok {
			continue
		}
		if got := fmt.Sprint(value["enum"]); got != "[low standard promotion high]" {
			t.Errorf("%s priority classes %s", tool["name"], got)
		}
	}
}
