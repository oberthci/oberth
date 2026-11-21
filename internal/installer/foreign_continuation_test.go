package installer_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/storage"
	"io"
	"testing"
)

type changedForeignContinuation struct {
	installer.ChartPort
	t     *testing.T
	store *storage.Storage
	armed bool
}

func (c *changedForeignContinuation) Read(ctx context.Context, ns string) (installer.History, error) {
	h, err := c.ChartPort.Read(ctx, ns)
	if err == nil && c.armed && h.Latest.RollbackID != "" && h.Latest.Status == "deployed" {
		c.armed = false
		appendForeignRevisions(c.t, c.store)
		return c.ChartPort.Read(ctx, ns)
	}
	return h, err
}
func TestForeignRecordedRepairRejectsNewOutsideChanges(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	_, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.Endpoint {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("fixture did not interrupt", err)
	}
	// A target that serves ready is completed, not repaired; this one has stopped.
	ready := f.engine.CheckEndpoint
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("target stopped") }
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Plan.Close()
	f.engine.CheckEndpoint = ready
	c := &changedForeignContinuation{ChartPort: f.engine.Charts, t: t, store: store, armed: true}
	f.engine.Charts = c
	_, err = m.Upgrade(t.Context(), next, next.Plan.Digest(), nil)
	var refusal *installer.Problem
	if c.armed {
		t.Fatal("fixture did not change outside history after rollback")
	}
	if !errors.As(err, &refusal) || refusal.Code != "target" || kc.updates != 2 {
		t.Fatal("new foreign history after rollback used the old approval", err, kc.updates)
	}
}
