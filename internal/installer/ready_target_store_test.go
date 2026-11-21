package installer_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

// An upgrade that failed at the store step is not completed while the store
// stays sealed, and both refusals name oberth unseal.
func TestSealedStoreKeepsAReadyTargetFromCompleting(t *testing.T) {
	t.Parallel()
	f, m, target, unsealed := sealedStoreUpgrade(t)
	before, history, applies := directoryBytes(t, f.dir), f.charts.History, len(f.charts.Applies)
	_, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	assertStoreSealed(t, err)
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
		t.Fatal("refused review changed the installation")
	}
	sealed := f.engine.OpenStore
	f.engine.OpenStore = unsealed
	complete, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("unsealed store still refused", err)
	}
	defer complete.Plan.Close()
	if !strings.Contains(strings.Join(complete.Plan.Review(), "\n"), "Recovery: complete the ready upgrade") {
		t.Fatal("unsealed ready target not reviewed for completion")
	}
	f.engine.OpenStore = sealed
	_, err = m.Upgrade(t.Context(), complete, complete.Plan.Digest(), nil)
	assertStoreSealed(t, err)
	if readUpgradeRecord(t, f).State != "pending" || len(f.charts.Applies) != applies {
		t.Fatal("refused completion changed the installation")
	}
}

// sealedStoreUpgrade leaves an upgrade that failed at the store step with the
// target deployed and serving; it returns the store opener of the unsealed store.
func sealedStoreUpgrade(t *testing.T) (fixture, installer.Maintenance, installer.Release, func(context.Context, secretstore.Config) (*secretstore.Client, func(), error)) {
	t.Helper()
	f, m := maintenanceFixture(t)
	target := loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	unsealed := f.engine.OpenStore
	store := &storefake.Store{Initialized: true, Sealed: true}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: store})
		return c, func() {}, err
	}
	_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "upgrade-failed" || !strings.Contains(refusal.Detail, "oberth unseal") {
		t.Fatal("store failure does not name oberth unseal", err)
	}
	f.engine.CheckEndpoint = func(_ context.Context, p installer.Profile) error {
		if p.ServerVersion != f.charts.History.Latest.Version {
			return errors.New("another version")
		}
		return nil
	}
	return f, m, target, unsealed
}

func assertStoreSealed(t *testing.T, err error) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "store-sealed" || !strings.Contains(refusal.Remedy, "oberth unseal") {
		t.Fatal("a sealed store is not refused with oberth unseal", err)
	}
}
