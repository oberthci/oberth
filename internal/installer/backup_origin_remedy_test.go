package installer

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestOriginRefusalNamesRecoveryRelease(t *testing.T) {
	t.Parallel()
	_, err := readBackupOrigin(strings.NewReader(`{"origin":{"unknown_format":true}}`))
	refusal := publicerr.Classify(err)
	if err == nil || !strings.Contains(refusal.Remedy(), "Restore with the same or a newer release") ||
		!strings.Contains(refusal.Remedy(), "--sha256") || len(refusal.Remedy()) > 120 || len(refusal.Reason()) > 80 {
		t.Fatal("origin refusal hid the recovery release or digest remedy", err)
	}
}
