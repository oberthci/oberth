package argoworkflow

import (
	"fmt"
	"os"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
)

//nolint:gochecknoglobals // All case directories use the same immutable oracle executable.
var controllerFixture = controllertest.New(map[string]string{
	"oberth_controller_test.go":       "testdata/controller_test.go",
	"oberth_source_driver_test.go":    "testdata/source_driver_test.go",
	"oberth_source_copy_test.go":      "testdata/source_copy_test.go",
	"oberth_installer_status_test.go": "testdata/installer_status_test.go",
	"oberth_cancelled_locks_test.go":  "testdata/cancelled_locks_test.go",
})

func TestMain(m *testing.M) {
	code := m.Run()
	if err := controllerFixture.Close(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "remove controller fixture:", err)
		code = 1
	}
	os.Exit(code)
}
