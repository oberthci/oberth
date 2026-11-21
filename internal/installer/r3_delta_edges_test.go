package installer_test

import (
	"context"

	"errors"
	"os"

	"testing"

	"github.com/oberthci/oberth/internal/installer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeltaFailedFirstInstallLocalEligibility(t *testing.T) {
	t.Parallel()
	for _, verified := range []bool{false, true} {
		t.Run(boolText(verified), func(t *testing.T) {
			f := setup(t)
			if verified {
				f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
			}
			f.engine.Charts = interruptedChart{ChartPort: f.charts, history: &f.charts.History}
			p := prepare(t, f)
			if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); !errors.Is(err, context.Canceled) {
				t.Fatal("setup did not interrupt", err)
			}
			f.engine.Charts = f.charts
			local := localVersion(t, f.release.Version(), 2)
			p, err := f.engine.Prepare(t.Context(), local, f.choices)
			if verified {
				if err == nil {
					p.Close()
					t.Fatal("failed verified install accepted local")
				}
				return
			}
			if err != nil {
				t.Fatal("failed local install cannot recover", err)
			}
			defer p.Close()
			apply(t, f, p)
		})
	}
}
func TestDeltaRemovedInstallationAllowsFreshLocal(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
	apply(t, f, prepare(t, f))
	// Model actual removal in the offline client: no remaining identities or
	// history, then reuse the namespace with a clean local profile directory.
	list, err := f.client.CoreV1().Secrets("oberth").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Items {
		if err = f.client.CoreV1().Secrets("oberth").Delete(t.Context(), s.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.client.CoreV1().Namespaces().Delete(t.Context(), "oberth", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.charts.History = installer.History{}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	f.engine.Credentials = credentials
	f.engine.OpenStore = setup(t).engine.OpenStore
	f.release = localVersion(t, "dev", 2)
	apply(t, f, prepare(t, f))
}

func deltaAssertOnlyClusterReads(t *testing.T, f fixture, actions int) {
	t.Helper()
	for _, a := range f.client.Actions()[actions:] {
		if !readOnly(a) {
			t.Fatalf("bad backup made cluster write: %s", a.GetVerb())
		}
	}
}
