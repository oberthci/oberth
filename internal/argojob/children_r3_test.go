package argojob_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type refusedScheduleLookup struct {
	argojob.ChildRuns
	err error
}

func (s refusedScheduleLookup) ScheduleAdmission(context.Context, int64, string, string) (admissioncontract.PreparedView, error) {
	return admissioncontract.PreparedView{}, s.err
}

func TestRestoreRetryUsesAdmissionExpiryKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		code             publicerr.Code
		expired, current bool
		want             publicerr.Code
	}{
		{"current_expiry_with_other_code", publicerr.Conflict, true, true, publicerr.Unavailable},
		{"current_unrelated_forbidden", publicerr.Forbidden, false, true, publicerr.Forbidden},
		{"older_expiry", publicerr.Forbidden, true, false, publicerr.Forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChildFixture(t)
			if !tc.current {
				advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
			}
			var cause error
			if tc.expired {
				cause = stateport.ScheduleAdmissionExpiredError{}
			}
			refusal := fmt.Errorf("lookup: %w", publicerr.New(tc.code, "admission lookup refused", "Check schedule access.", cause))
			c := f.executorConfig()
			c.ChildRuns = refusedScheduleLookup{f.s, refusal}
			err := argojob.New(c).RecordChildForTest(t.Context(), f.child)
			if err == nil || publicerr.Classify(err).Code() != tc.want {
				t.Fatal("wrong lookup classification", err)
			}
			if tc.want == publicerr.Unavailable && publicerr.Classify(err).Reason() != "scheduled admission not found" {
				t.Fatal("expiry retry lost its reason", err)
			}
		})
	}
}

func TestPredecessorMetadataStaysOnNativeParent(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	original := f.prepared.View().Digest
	advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	var err error
	f.parent, err = f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if f.parent.Annotations["oberth.ci/previous-admission"] != original {
		t.Fatal("native parent lost predecessor", f.parent.Annotations)
	}
	child := f.fire(t)
	if _, present := child.Annotations["oberth.ci/previous-admission"]; present {
		t.Fatal("parent history was copied into the child")
	}
	version := f.parent.ResourceVersion
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	parent, err := f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if parent.ResourceVersion != version || parent.Annotations["oberth.ci/previous-admission"] != original {
		t.Fatal("unchanged apply rewrote parent history", parent.Annotations)
	}
}
