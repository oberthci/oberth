package gitport

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestForgeRefusalIsShortAndKeepsNativeDetailPrivate(t *testing.T) {
	native := errors.New("private native output")
	err := &UnavailableError{Op: "forge", Cause: native}
	if !errors.Is(err, native) || strings.Contains(err.Error(), "private") || publicerr.Classify(err).Reason() != ForgeReason {
		t.Fatal(err)
	}
	oldest := time.Duration(1<<63 - 1).Round(time.Second).String()
	for text, bound := range map[string]int{ForgeStatusReason: 40, EmptyForgeReason: 40, EmptyForgeRemedy: 120, ForgeReason: 40, ForgeRemedy: 120, ForgeOperatorRemedy: 120, ForgeCacheNotice + oldest + ".": 80} {
		if len(text) > bound || strings.Contains(text, "...") {
			t.Fatal("message exceeds budget", len(text), text)
		}
	}
}
