package argojob_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
)

//nolint:gochecknoglobals // Oracle cases have private inputs and share a read-only executable.
var holdControllerFixture = controllertest.New(map[string]string{
	"oberth_hold_test.go":          "testdata/hold_invocations_controller_test.go",
	"oberth_secret_helper_test.go": "testdata/secret_helper_controller_test.go",
})

func TestMain(m *testing.M) {
	code := m.Run()
	if err := holdControllerFixture.Close(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "remove controller fixture:", err)
		code = 1
	}
	os.Exit(code)
}
