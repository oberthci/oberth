package installer_test

import (
	"os"
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

// An update keeps the deploy key, repositories and identities: it prints no
// first-run step, and the reused administrator key keeps its file.
func TestUpdateKeepsTheFirstRunAndTheAdministratorKeyFile(t *testing.T) {
	t.Parallel()
	f := setup(t)
	first := apply(t, f, prepare(t, f))
	second := apply(t, f, prepare(t, f))
	if first.Updated || !second.Updated || second.Next != installer.UpdateNext || len(second.Next) > 120 {
		t.Fatal("update repeated the first run", first.Updated, second.Updated, second.Next)
	}
	if second.Profile.AdminKey != first.Profile.AdminKey {
		t.Fatal("reused administrator key renamed", first.Profile.AdminKey, second.Profile.AdminKey)
	}
	if _, err := os.Stat(second.Profile.AdminKey); err != nil {
		t.Fatal("reused administrator key file lost", err)
	}
}
