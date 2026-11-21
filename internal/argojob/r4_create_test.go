package argojob_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCreatePolicyAndQuotaUseTypedStatusBeforeMessage(t *testing.T) {
	t.Parallel()
	denied := func(reason meta.StatusReason, code int32, cause meta.CauseType, message string) error {
		return &apierrors.StatusError{ErrStatus: meta.Status{Reason: reason, Code: code, Message: message, Details: &meta.StatusDetails{Causes: []meta.StatusCause{{Type: cause}}}}}
	}
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
		code      publicerr.Code
	}{
		{"typed invalid policy", denied(meta.StatusReasonInvalid, 422, "", "opaque denial"), true, publicerr.InvalidInput},
		{"typed forbidden field", denied(meta.StatusReasonForbidden, 403, meta.CauseTypeForbidden, "opaque denial"), true, publicerr.Forbidden},
		{"wrapped typed forbidden", fmt.Errorf("native: %w", denied(meta.StatusReasonForbidden, 403, meta.CauseTypeForbidden, "opaque denial")), true, publicerr.Forbidden},
		{"typed namespace termination", denied(meta.StatusReasonForbidden, 403, core.NamespaceTerminatingCause, "cannot create resource while closing"), false, publicerr.Unavailable},
		{"typed throttling", denied(meta.StatusReasonTooManyRequests, 429, meta.CauseTypeForbidden, `admission webhook "policy" denied the request`), false, publicerr.Unavailable},
		{"quota", apierrors.NewForbidden(resource, "run", errors.New("exceeded quota: builds")), false, publicerr.Unavailable},
		{"forbidden policy", apierrors.NewForbidden(resource, "run", errors.New("ValidatingAdmissionPolicy 'policy' with binding 'binding' denied request: rejected")), true, publicerr.Forbidden},
		{"forbidden webhook", denied(meta.StatusReasonForbidden, 403, "", `admission webhook "policy" denied the request: rejected`), true, publicerr.Forbidden},
		{"unknown forbidden", apierrors.NewForbidden(resource, "run", errors.New("opaque refusal")), false, publicerr.Unavailable},
		{"wrapper message is not the status", fmt.Errorf("cannot create resource: %w", apierrors.NewForbidden(resource, "run", errors.New("opaque refusal"))), false, publicerr.Unavailable},
		{"transport text is not status", errors.New(`admission webhook "policy" denied the request`), false, publicerr.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e, f := fixture(t)
			f.CreateError = tc.err
			b, err := e.Create(t.Context(), p)
			if err == nil || publicerr.Permanent(err) != tc.permanent || publicerr.Classify(err).Code() != tc.code || b.Workflow.UID != "" {
				t.Fatal("wrong Create disposition", b, err, tc.permanent, tc.code)
			}
			wantReads := 1
			if tc.permanent {
				wantReads = 0
			}
			if f.Gets != wantReads {
				t.Fatal("definitive refusal used uncertain lookup", f.Gets, wantReads)
			}
		})
	}
}
