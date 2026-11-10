package installer

import (
	"testing"

	"github.com/oberthci/oberth/internal/service"
)

func TestRestoredRunsMarkerNameMatchesTheServer(t *testing.T) {
	t.Parallel()
	if restoredRunsFile != service.RestoredRunsFile {
		t.Fatalf("restore writes %q, the server reads %q", restoredRunsFile, service.RestoredRunsFile)
	}
}
