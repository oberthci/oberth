package installer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
)

// The native library statuses are reviewed before a mutation is authorized.
func TestEveryReleaseStateHasANextAction(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ state, action string }{
		{"", "install"}, {"deployed", "apply"}, {"failed", "apply"},
		{"pending-install", "repair"}, {"pending-upgrade", "repair"}, {"pending-rollback", "repair"},
		{"uninstalling", "finish removing"}, {"uninstalled", "finish removing"},
		{"unknown", "refuse"}, {"superseded", "apply"},
	} {
		t.Run(row.state, func(t *testing.T) { releaseStateCase(t, row.state, row.action) })
	}
}
func releaseStateCase(t *testing.T, state, action string) {
	t.Helper()
	f := setup(t)
	if state != "" {
		f.release = loadFixture(t, releaseFixture(t))
		apply(t, f, prepare(t, f))
		values := f.charts.History.Latest.Values
		f.charts.History = installer.History{}
		f.charts.History.Latest = installer.Revision{Number: 1, Status: state, Kind: "verified", Version: "v0.1.0", Values: values}
		f.release = loadFixture(t, releaseFixture(t))
	}
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if action == "refuse" {
		if err == nil {
			p.Close()
			t.Fatal("unsupported latest status accepted")
		}
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "chart-state" || !strings.Contains(err.Error(), state) {
			t.Fatal("unsupported state remedy is unclear", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if action == "finish removing" && !strings.Contains(strings.Join(p.Review(), "\n"), "revision 1") {
		t.Fatal(p.Review())
	}
	apply(t, f, p)
	request := f.charts.Applies[len(f.charts.Applies)-1]
	repair := action == "repair" || action == "finish removing"
	if (request.Recovery != nil) != repair {
		t.Fatal("wrong next action", action, request.Recovery)
	}
}

func TestNativeRepairRechecksOwnershipAfterReview(t *testing.T) {
	t.Parallel()
	port, store, request := pendingFixture(t, false)
	if err := port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	last.Labels["oberth.ci/installer"] = "false"
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	if err = port.Apply(t.Context(), request); err == nil {
		t.Fatal("repair applied after ownership changed")
	}
	last, err = store.Last("oberth")
	if err != nil || last.Version != 1 || !last.Info.Status.IsPending() {
		t.Fatal("refusal mutated release", last, err)
	}
}

func TestUninstalledHistoryUsesFreshChart(t *testing.T) {
	t.Parallel()
	for _, status := range []release.Status{release.StatusUninstalling, release.StatusUninstalled} {
		port, store, request := pendingFixture(t, false)
		last, err := store.Last("oberth")
		if err != nil {
			t.Fatal(err)
		}
		last.Info.Status = status
		if err = store.Update(last); err != nil {
			t.Fatal(err)
		}
		if err = port.Apply(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		last, err = store.Last("oberth")
		if err != nil || last.Version != 2 || last.Info.Status != release.StatusDeployed {
			t.Fatal("fresh chart did not replace unfinished removal", err)
		}
	}
}

func TestForeignUnfinishedRemovalRefuses(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"uninstalling"} {
		f := setup(t)
		f.charts.History.Latest = installer.Revision{Number: 1, Status: state}
		if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
			p.Close()
			t.Fatal("foreign removal adopted", state)
		}
	}
}
