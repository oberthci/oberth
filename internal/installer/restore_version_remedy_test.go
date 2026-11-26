package installer_test

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRestoreReviewAndGuideGiveReleaseRemedy(t *testing.T) {
	_, err := installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{})
	remedy := strings.ToLower(publicerr.Classify(err).Remedy())
	if err == nil || !strings.Contains(remedy, "restore with the same or a newer release") {
		t.Fatal("restore review hid its version remedy", err)
	}
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil || !strings.Contains(strings.ToLower(string(raw)), "restore with the same or a newer release") || !strings.Contains(string(raw), "v0.30.17 cannot read archives with forge host pins") {
		t.Fatal("restore guide omits the archive version boundary", err)
	}
}
