package installer_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
)

func TestReviewNamesSavedChoicesAfterOutsideUpgradeAndRemoval(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	nativeReuseValuesUpgrade(t, store, kc)
	unchanged := unchangedInstallation(t, f, store, kc)
	p := prepare(t, f)
	assertSavedChoiceSource(t, p)
	p.Close()
	unchanged()
	u := action.NewUninstall(&action.Configuration{Releases: store, KubeClient: kc, Log: func(string, ...any) {}})
	u.KeepHistory = true
	if _, err := u.Run("oberth"); err != nil {
		t.Fatal("native keep-history uninstall failed", err)
	}
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Latest.Number != 2 || h.Latest.Status != "uninstalled" || h.Latest.Installer() || h.Deployed.Number != 0 || h.Choices.Number != 1 {
		t.Fatal("outside removal changed the saved-choice source", err)
	}
	unchanged = unchangedInstallation(t, f, store, kc)
	p = prepare(t, f)
	defer p.Close()
	assertSavedChoiceSource(t, p)
	if !slices.Contains(p.Review(), "Removed revision 2") {
		t.Fatal("review does not distinguish removed revision from saved choices", p.Review())
	}
	unchanged()
	apply(t, f, p)
	assertStoredParallel(t, store, 3)
	last, err := store.Last("oberth")
	if err != nil || last.Version != 3 || last.Info.Status != release.StatusDeployed {
		t.Fatal("reviewed recovery did not replace the removed outside revision", err)
	}
}

func assertSavedChoiceSource(t *testing.T, p *installer.Plan) {
	t.Helper()
	lines := p.Review()
	text := strings.Join(lines, "\n")
	if !slices.Contains(lines, "Saved choices: revision 1 (superseded)") || strings.Count(text, "Saved choices:") != 1 || !strings.Contains(text, "Parallel runs: 3 ") || strings.Contains(text, "reapply saved choices") {
		t.Fatal("review does not identify the installer revision supplying choices", text)
	}
}
