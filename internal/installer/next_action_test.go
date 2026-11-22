package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// A new installation's next action names the forge URL shape repo add accepts,
// before the person meets a refusal of another SSH user or a deeper path.
func TestFirstInstallNamesTheAcceptedForgeURL(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := apply(t, f, prepare(t, f))
	if r.Next != installer.FirstRunNext || !strings.Contains(r.Next, "repo add git@<forge>:<organization>/<repository>.git") {
		t.Fatal("first run hides the forge URL shape", r.Next)
	}
	if len(r.Next) > 120 {
		t.Fatal("next action exceeds the remedy bound", len(r.Next))
	}
}
