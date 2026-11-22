package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
)

// A cancelled release still used its version, so the floor refusal prints no
// cancel command to follow.
func TestReleaseFloorRefusalPrintsNoCancelCommand(t *testing.T) {
	refused := &app.Refusal{Code: "release_old"}
	if remedy := refused.Remedy(); strings.Contains(remedy, "cancel") || strings.Contains(remedy, "oberth ") {
		t.Fatal("the release floor refusal offers a version-spending command", remedy)
	}
}
