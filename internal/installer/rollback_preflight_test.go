package installer_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
)

func TestNativeRollbackPreflightIsReadOnly(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"invalid chart", "changed owner", "changed namespace", "changed revision"} {
		t.Run(kind, func(t *testing.T) { nativeRollbackRefusal(t, kind) })
	}
}
func nativeRollbackRefusal(t *testing.T, kind string) {
	t.Helper()

	port, store, request := pendingFixture(t, true)
	request.Revision = request.Recovery.Pending
	request.Recovery = nil
	request.RollbackID = "reviewed"
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	changeNativeRollback(last, &request, kind)
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	if err = port.Apply(t.Context(), request); err == nil {
		t.Fatal("unsafe verified chart apply accepted")
	}
	after, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(after)
	if err != nil || !bytes.Equal(before, raw) || after.Info.Status != release.StatusPendingUpgrade {
		t.Fatal("preflight refusal mutated pending revision", err)
	}
}
func TestNativeRollbackUnlocksOwnedPendingRevisionWithFreshChart(t *testing.T) {
	t.Parallel()
	port, store, request := pendingFixture(t, true)
	request.Revision = request.Recovery.Pending
	request.Recovery = nil
	request.RollbackID = "reviewed"
	if err := port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err := store.Last("oberth")
	if err != nil || last.Info.Status != release.StatusPendingUpgrade {
		t.Fatal("preview mutated pending revision", err)
	}
	if err = port.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err = store.Last("oberth")
	if err != nil || last.Version != request.Revision+1 || last.Info.Status != release.StatusDeployed || last.Labels["oberth.ci/rollback"] != "reviewed" {
		t.Fatal("fresh rollback did not replace exact pending revision", err)
	}
}

func changeNativeRollback(last *release.Release, request *installer.ChartRequest, kind string) {
	switch kind {
	case "invalid chart":
		request.Chart.Schema = []byte(`{"type":"object","required":["missing"]}`)
	case "changed owner":
		last.Labels["oberth.ci/installer"] = "false"
	case "changed namespace":
		request.Namespace = "another"
	case "changed revision":
		request.Revision++
	}
}
