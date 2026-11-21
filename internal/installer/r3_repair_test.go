package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
)

func TestNativeRepairRechecksDeployedTargetAfterReview(t *testing.T) {
	t.Parallel()
	port, store, request := pendingFixture(t, true)
	if err := port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	target, err := store.Get("oberth", request.Recovery.Deployed)
	if err != nil {
		t.Fatal(err)
	}
	target.Info.Status = release.StatusSuperseded
	if err = store.Update(target); err != nil {
		t.Fatal(err)
	}
	before, err := store.History("oberth")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err = port.Apply(t.Context(), request); err == nil {
		t.Fatal("changed rollback target accepted")
	}
	after, err := store.History("oberth")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(after)
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal("refusal changed native release history", err)
	}
}

func TestRepairApprovalBindsDeployedRevisionKind(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	apply(t, f, prepare(t, f))
	installerAttempt(t, store, release.StatusPendingUpgrade)
	p := prepare(t, f)
	defer p.Close()
	changeRevision(t, store, 2, func(r *release.Release) { r.Labels["oberth.ci/installer"] = "false" })
	unchanged := unchangedInstallation(t, f, store, kc)
	var refusal *installer.Problem
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); !errors.As(err, &refusal) || refusal.Code != "target" {
		t.Fatal("approval accepted a changed deployed revision kind", err)
	}
	unchanged()
}
