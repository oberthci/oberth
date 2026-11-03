package integration

import (
	"path/filepath"
	"testing"
)

// The inactive pilot is separate from the still-supported legacy execution
// engine. Its historical SQL journal remains, but no runtime ships for it.
func TestInactivePilotRuntimeIsRemoved(t *testing.T) {
	for _, pattern := range []string{"pilot*.go", "conductor*.go"} {
		matches, err := filepath.Glob(filepath.Join("..", "..", "internal", "vmrunner", pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Errorf("inactive pilot runtime still ships: %v", matches)
		}
	}
}
