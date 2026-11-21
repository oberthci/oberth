package installer_test

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

// An ordinary upgrade whose store the apply would refuse is refused in its
// review, before any change, for the same and for a new version.
func TestSealedStoreRefusesOrdinaryUpgradeInReview(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "v0.1.0"} {
		t.Run("version "+version, func(t *testing.T) {
			f, m := maintenanceFixture(t)
			target := f.release
			if version != "" {
				target = loadFixture(t, releaseFor(t, version, 1))
			}
			sealStore(f)
			before, history, applies := directoryBytes(t, f.dir), f.charts.History, len(f.charts.Applies)
			_, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
			assertStoreSealed(t, err)
			if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
				t.Fatal("refused review changed the installation")
			}
		})
	}
}

// A store Pod that never becomes Ready is left to the apply: the review returns
// within its bound and goes on, saying nothing about the store.
func TestUnreadableStoreLeavesOrdinaryUpgradeToTheApply(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.engine.OpenStore = nil
	// The caller's deadline is three times the store bound; the review must end first.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	plan, err := m.PlanUpgrade(ctx, f.release, f.choices, installer.UpgradeOptions{})
	if ctx.Err() != nil {
		t.Fatal("review waited for the store until its caller's deadline")
	}
	if err != nil {
		t.Fatal("review refused a store it could not read", err)
	}
	plan.Plan.Close()
}

// Without the bundled store there is no store step, so nothing is checked.
func TestExternalStoreUpgradeReviewOpensNoStore(t *testing.T) {
	t.Parallel()
	f := setup(t)
	external := false
	f.choices.Store = &external
	f.choices.StoreAddress = "https://external.oberth.svc"
	apply(t, f, prepare(t, f))
	f.charts.History.Deployed.Version = "v0.0.9"
	f.charts.History.Latest = f.charts.History.Deployed
	setInstalledMetadata(t, f, "local", 1)
	source, options := backupFixture(t)
	m := installer.Maintenance{Engine: f.engine, Source: source, Options: options, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		return releaseFor(t, version, 1).files, ctx.Err()
	}, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		return nil, nil, errors.New("external installation opened the bundled store")
	}
	plan, err := m.PlanUpgrade(t.Context(), loadFixture(t, releaseFixture(t)), f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Plan.Close()
}

func sealStore(f fixture) {
	sealed := &storefake.Store{Initialized: true, Sealed: true}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: sealed})
		return c, func() {}, err
	}
}
