package installer_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRemovedInstallationKeptProfileRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History = installer.History{}
	for _, name := range []string{"oberth-admin", "oberth-ca", "oberth-forge-key", "oberth-ssh-host", "oberth-store-tls"} {
		if err := f.client.CoreV1().Secrets("oberth").Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	for range 2 {
		f.client.ClearActions()
		p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
		if p != nil {
			p.Close()
		}
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "profile-ca" {
			t.Fatal("kept profile accepted", err)
		}
		for _, action := range f.client.Actions() {
			if !readOnly(action) {
				t.Fatal("refusal wrote cluster state", action)
			}
		}
		if len(f.charts.Applies) != applies || !reflect.DeepEqual(before, directoryBytes(t, f.dir)) {
			t.Fatal("refusal changed profile or chart")
		}
	}
}
