package installer_test

import (
	"errors"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEarlierInstallationNamespaceRefusalBeforeWrites(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	changeRevision(t, store, 1, func(r *release.Release) { r.Labels = nil })
	if err := f.client.CoreV1().Secrets("oberth").Delete(t.Context(), "oberth-admin", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	freshDir := replaceOwnerCredentials(t, f, installer.Profile{}, "absent")
	unchanged := unchangedInstallation(t, f, store, kc)
	_, err := f.engine.Prepare(t.Context(), f.release, installer.Choices{Context: f.choices.Context})
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "namespace-installation" || refusal.Reason != "This namespace holds another installation." || refusal.Remedy != "Choose another --namespace and unused --https-node-port and --ssh-node-port." {
		t.Fatal("another installation was described as damage", err)
	}
	unchanged()
	if _, err := os.Stat(freshDir); !os.IsNotExist(err) {
		t.Fatal("refusal created a fresh profile directory", err)
	}
}
