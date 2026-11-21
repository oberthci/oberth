package installer_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestIdentityPersistenceComparesCreatedReadback(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	f.client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		secret := action.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		if secret.Name == "oberth-ca" {
			secret.Data["tls.crt"] = []byte("changed after preparation")
		}
		return false, nil, nil
	})
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(errors.Unwrap(err), &refusal) || refusal.Code != "target" || len(f.charts.Applies) != 0 {
		t.Fatal("created identity was not compared before chart apply", err)
	}
}
