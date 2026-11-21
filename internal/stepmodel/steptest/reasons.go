package steptest

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// AssertReasons checks a component's complete run/step reason catalogue. Empty
// means no reason; messages must fit 40 characters without ellipses. Native
// diagnostics belong in Detail, never this catalogue.
func AssertReasons(t *testing.T, reasons ...string) {
	t.Helper()
	for _, reason := range reasons {
		if err := publicerr.CheckMessage(reason, 40); err != nil {
			t.Errorf("status reason %q: %v", reason, err)
		}
	}
}
