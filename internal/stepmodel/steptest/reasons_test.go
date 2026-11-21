package steptest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestReasonHelperAcceptsCompletePhrasesAndCharacterBoundary(t *testing.T) {
	AssertReasons(t, "", "image pull failed", "waiting for a node", "step timed out after 10m", strings.Repeat("a", 40), strings.Repeat("é", 40))
}

func TestReasonHelperRefusesOverlongOrIncompleteReasons(t *testing.T) {
	for _, reason := range []string{strings.Repeat("a", 41), strings.Repeat("é", 41), "waiting...", "waiting…", "waiting... ", "waiting… ", "waiting\nfor a node", "waiting\rfor a node"} {
		if publicerr.CheckMessage(reason, 40) == nil {
			t.Errorf("accepted incomplete or overlong reason %q", reason)
		}
	}
}
