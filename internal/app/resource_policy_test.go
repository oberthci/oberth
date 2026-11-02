package app

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type resourceGrantAudit struct {
	service.Auditor
	maximum periapsis.Size
	err     error
	ids     []int64
}

func (a *resourceGrantAudit) MaxRunSize(_ context.Context, id int64) (periapsis.Size, error) {
	a.ids = append(a.ids, id)
	return a.maximum, a.err
}
func TestResourceGrantBindingIsFreshScopedAndFailsClosed(t *testing.T) {
	a := &resourceGrantAudit{maximum: periapsis.XL}
	jobs := &ArgoJobs{auditor: a}
	submission := argojob.Request{Repo: "repo-01", RepositoryID: 17}
	if e := jobs.bindResourceMaximum(t.Context(), &submission); e != nil || submission.ResourceMaximum != periapsis.XL {
		t.Fatalf("grant: %v", e)
	}
	a.maximum = periapsis.L
	if e := jobs.bindResourceMaximum(t.Context(), &submission); e != nil || submission.ResourceMaximum != periapsis.L {
		t.Fatalf("revocation: %v", e)
	}
	a.err = errors.New("unavailable")
	if e := jobs.bindResourceMaximum(t.Context(), &submission); e == nil {
		t.Fatal("grant lookup failed open")
	}
	if len(a.ids) != 3 || a.ids[0] != 17 || a.ids[1] != 17 || a.ids[2] != 17 {
		t.Fatal("grant repository binding")
	}
}
