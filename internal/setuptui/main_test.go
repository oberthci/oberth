package setuptui

import (
	"os"
	"testing"
)

// TestMain stubs the helm/kubectl check: the wizard's tests must not depend
// on which tools the machine running them has installed (CI has no kubectl).
func TestMain(m *testing.M) {
	checkHostTools = func(string, func(string) (string, error)) error { return nil }
	os.Exit(m.Run())
}
