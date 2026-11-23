package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestOnlyConfiguredDefaultPublicationBootstrapsEmptyForge(t *testing.T) {
	for _, ref := range []string{"refs/heads/trunk", "refs/heads/main", "refs/heads/topic"} {
		t.Run(ref, func(t *testing.T) {
			checkEmptyPublication(t, ref)
		})
	}
}

func checkEmptyPublication(t *testing.T, ref string) {
	t.Helper()
	f := setup(t)
	f.source.Default = func(context.Context, string) (string, error) { return "refs/heads/trunk", nil }
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.EmptyRepositoryError{}
	}
	r := f.seed(stepmodel.RunRunning)
	r.Ref = ref
	p := finishPublication(t, f, r)
	if !f.timers.Fire() {
		t.Fatal("publication was not armed")
	}
	workerOK(t, f)
	p, err := f.store.Publication(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref == "refs/heads/trunk" {
		if p.Status != model.PublicationDelivered || p.PreviousSHA == nil || *p.PreviousSHA != "" {
			t.Fatal("configured default could not bootstrap", p)
		}
	} else if p.Status != model.PublicationFailed || !strings.Contains(p.Detail, gitport.EmptyForgeRemedy) || p.RetryAt != nil || f.timers.Fire() {
		t.Fatal("topic bootstrap was not refused with a remedy", p)
	}
}
