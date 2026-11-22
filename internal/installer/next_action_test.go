package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A fresh install whose server never became ready leaves a deployed release;
// its retry is still the first run and names the first-run step.
func TestRetriedFreshInstallIsStillTheFirstRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.engine.Timeout = 50 * time.Millisecond
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("server not ready") }
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("fixture: an unready server completed the install")
	}
	if f.charts.History.Deployed.Number == 0 {
		t.Fatal("fixture: the failed install left no deployed release")
	}
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	retry := apply(t, f, prepare(t, f))
	if retry.Updated || retry.Next != installer.FirstRunNext {
		t.Fatal("retried fresh install claimed an update", retry.Updated, retry.Next)
	}
	if again := apply(t, f, prepare(t, f)); !again.Updated || again.Next != installer.UpdateNext {
		t.Fatal("apply after a completed install is not an update", again.Updated, again.Next)
	}
}

// A crash while the stable key name was first written leaves a partial file
// there while the profile still names the earlier, intact key file. The next
// apply replaces the partial file whole instead of refusing.
func TestPartialAdministratorKeyFileIsReplaced(t *testing.T) {
	t.Parallel()
	f := setup(t)
	first := apply(t, f, prepare(t, f))
	key, err := os.ReadFile(first.Profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	earlier := filepath.Join(f.dir, "admin-"+strings.Repeat("e", 64)+".key")
	if err = os.WriteFile(earlier, key, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(first.Profile.AdminKey, key[:len(key)/2], 0600); err != nil {
		t.Fatal(err)
	}
	profile := first.Profile
	profile.AdminKey = earlier
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.dir, "installation.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	second := apply(t, f, prepare(t, f))
	if replaced, err := os.ReadFile(second.Profile.AdminKey); err != nil || second.Profile.AdminKey != first.Profile.AdminKey || !bytes.Equal(replaced, key) {
		t.Fatal("partial key file was not replaced whole", err)
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
