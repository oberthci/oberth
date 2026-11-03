package app

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/pipeline"
	corev1 "k8s.io/api/core/v1"
)

type resourceGrantAudit struct {
	service.Auditor
	maximum pipeline.Size
	err     error
	ids     []int64
}

func (a *resourceGrantAudit) MaxRunSize(_ context.Context, id int64) (pipeline.Size, error) {
	a.ids = append(a.ids, id)
	return a.maximum, a.err
}
func TestResourceGrantBindingIsFreshScopedAndFailsClosed(t *testing.T) {
	a := &resourceGrantAudit{maximum: pipeline.XL}
	jobs := &ArgoJobs{auditor: a}
	submission := argojob.Request{Repo: "repo-01", RepositoryID: 17}
	if e := jobs.bindResourceMaximum(t.Context(), &submission); e != nil || submission.ResourceMaximum != pipeline.XL {
		t.Fatalf("grant: %v", e)
	}
	a.maximum = pipeline.L
	if e := jobs.bindResourceMaximum(t.Context(), &submission); e != nil || submission.ResourceMaximum != pipeline.L {
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

type numericGrantAudit struct {
	service.Auditor
	ceiling corev1.ResourceRequirements
	err     error
	ids     []int64
}

func (a *numericGrantAudit) ResourceCeiling(_ context.Context, id int64) (corev1.ResourceRequirements, error) {
	a.ids = append(a.ids, id)
	return a.ceiling, a.err
}
func TestNumericResourceGrantBindingIsFreshScopedAndFailsClosed(t *testing.T) {
	a := &numericGrantAudit{ceiling: store.DefaultResourceCeiling()}
	jobs := &ArgoJobs{auditor: a}
	r := argojob.Request{Repo: "repo-01", RepositoryID: 17}
	if err := jobs.bindResourceMaximum(t.Context(), &r); err != nil {
		t.Fatal(err)
	}
	a.ceiling.Requests[corev1.ResourceCPU] = resource.MustParse("1250m")
	if r.ResourceCeiling.Requests.Cpu().MilliValue() != 4000 {
		t.Fatal("caller mutated snapshot")
	}
	if err := jobs.bindResourceMaximum(t.Context(), &r); err != nil || r.ResourceCeiling.Requests.Cpu().MilliValue() != 1250 {
		t.Fatalf("fresh lookup: %v", err)
	}
	a.err = errors.New("unavailable")
	if err := jobs.bindResourceMaximum(t.Context(), &r); err == nil {
		t.Fatal("numeric ceiling failed open")
	}
	for _, id := range a.ids {
		if id != 17 {
			t.Fatal("wrong repository grant")
		}
	}
}
