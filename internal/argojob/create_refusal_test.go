package argojob_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestCreateDefinitiveClusterRefusalsArePermanent(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	rows := []struct {
		name  string
		cause error
		code  publicerr.Code
	}{
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: resource.Group, Kind: "Workflow"}, "run", field.ErrorList{field.Invalid(field.NewPath("spec", "entrypoint"), "missing", "template does not exist")}), publicerr.InvalidInput},
		{"forbidden", apierrors.NewForbidden(resource, "run", errors.New(`User "server" cannot create resource "workflows" in API group "argoproj.io" in the namespace "ci"`)), publicerr.Forbidden},
		{"unauthorized", apierrors.NewUnauthorized("authentication required"), publicerr.Forbidden},
		{"invalid Accept", apierrors.NewGenericServerResponse(406, "create", resource, "run", "unsupported Accept header", 0, false), publicerr.InvalidInput},
		{"invalid Content-Type", apierrors.NewGenericServerResponse(415, "create", resource, "run", "unsupported Content-Type header", 0, false), publicerr.InvalidInput},
		{"too large", apierrors.NewRequestEntityTooLargeError("object exceeds the request limit"), publicerr.InvalidInput},
		{"bad request", apierrors.NewBadRequest("invalid request"), publicerr.InvalidInput},
		{"missing namespace", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "ci"), publicerr.InvalidInput},
		{"unsupported method", apierrors.NewMethodNotSupported(resource, "create"), publicerr.InvalidInput},
	}
	for _, row := range rows {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%v", row.name, wrapped), func(t *testing.T) {
				p, e, f := fixture(t)
				cause := row.cause
				if wrapped {
					cause = fmt.Errorf("native client: %w", cause)
				}
				f.CreateError = cause
				b, err := e.Create(t.Context(), p)
				if !publicerr.Permanent(err) || publicerr.Classify(err).Code() != row.code || !errors.Is(err, cause) || b.Workflow.UID != "" || f.Posts != 1 || f.Gets != 0 {
					t.Fatal("definitive refusal left pending", b, err, f.Posts, f.Gets)
				}
			})
		}
	}
}
func TestCreateTransientErrorsRemainUnavailable(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	rows := []struct {
		name  string
		cause error
	}{
		{"timeout", apierrors.NewTimeoutError("reply lost", 1)},
		{"server timeout", apierrors.NewServerTimeout(resource, "create", 1)},
		{"unavailable", apierrors.NewServiceUnavailable("API unavailable")},
		{"throttled", apierrors.NewTooManyRequests("retry later", 1)},
		{"server error", apierrors.NewInternalError(errors.New("storage unavailable"))},
		{"conflict", apierrors.NewConflict(resource, "run", errors.New("concurrent write"))},
		{"EOF", io.EOF}, {"unexpected EOF", io.ErrUnexpectedEOF},
		{"canceled", context.Canceled}, {"deadline", context.DeadlineExceeded},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p, e, f := fixture(t)
			f.CreateError = row.cause
			b, err := e.Create(t.Context(), p)
			if err == nil || publicerr.Permanent(err) || publicerr.Classify(err).Code() != publicerr.Unavailable || b.Workflow.UID != "" || f.Posts != 1 || f.Gets != 1 {
				t.Fatal("transient refusal made permanent or reposted", b, err, f.Posts, f.Gets)
			}
		})
	}
}
func TestCreateExistingForeignWorkflowIsPermanentWithoutOwnedBinding(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	_, err := argojob.New(argojob.Config{Runs: bindingRefusal{s}, Workflows: f}).Create(t.Context(), p)
	if err == nil {
		t.Fatal("fixture unexpectedly bound")
	}
	f.Object.Labels[nativebinding.RunLabel] = "foreign"
	b, err := argojob.New(argojob.Config{Runs: s, Workflows: f}).Create(t.Context(), p)
	var foreign *nativebinding.Refusal
	if !publicerr.Permanent(err) || !errors.As(err, &foreign) || b.Workflow.UID != "" || f.Posts != 2 || f.Gets != 1 {
		t.Fatal("foreign object presented as created", b, err, f.Posts, f.Gets)
	}
	run, readErr := s.Run(t.Context(), p.View().RunID)
	compositionOK(t, readErr)
	if run.Origin.WorkflowUID != "" {
		t.Fatal("foreign object bound to run", run.Origin)
	}
}
func TestCreateOwnedWorkflowRetainsUIDOnBindingRefusal(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	b, err := argojob.New(argojob.Config{Runs: permanentBindingRefusal{s}, Workflows: f}).Create(t.Context(), p)
	if !publicerr.Permanent(err) || b.Workflow.UID == "" || f.Posts != 1 || f.Gets != 0 {
		t.Fatal("created Workflow lost its recovery proof", b, err)
	}
	b, err = argojob.New(argojob.Config{Runs: s, Workflows: f}).Create(t.Context(), p)
	compositionOK(t, err)
	if b.Workflow.UID == "" || f.Posts != 2 || f.Gets != 1 {
		t.Fatal("owned AlreadyExists was refused", b, f.Posts, f.Gets)
	}
}

type createRunPort interface{ argojob.Runs }
type permanentBindingRefusal struct{ createRunPort }

func (permanentBindingRefusal) BindWorkflow(context.Context, stateport.Action, nativebinding.RunBinding) error {
	return publicerr.New(publicerr.Forbidden, "Workflow binding was refused", "Restore durable binding access.", nil)
}
func TestCreateReconciliationReadErrorsRemainUnavailable(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	for _, createErr := range []error{apierrors.NewAlreadyExists(resource, "run"), apierrors.NewTimeoutError("reply lost", 1)} {
		for _, getErr := range []error{apierrors.NewNotFound(resource, "run"), apierrors.NewForbidden(resource, "run", errors.New("read is denied")), apierrors.NewServiceUnavailable("read unavailable")} {
			t.Run(createErr.Error()+"/"+getErr.Error(), func(t *testing.T) {
				p, e, f := fixture(t)
				f.CreateError = createErr
				f.GetError = getErr
				b, err := e.Create(t.Context(), p)
				if !errors.Is(err, getErr) || publicerr.Permanent(err) || publicerr.Classify(err).Code() != publicerr.Unavailable || b.Workflow.UID != "" || f.Posts != 1 || f.Gets != 1 {
					t.Fatal("unverified create was made permanent", b, err, f.Posts, f.Gets)
				}
			})
		}
	}
}

func TestCreateQuotaAndTerminatingNamespaceRemainUnavailable(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	for _, detail := range []string{"exceeded quota: builds, requested: count/workflows=1, used: 2, limited: 2", "unable to create new content in namespace ci because it is being terminated", "admission temporarily refuses this Workflow"} {
		t.Run(detail, func(t *testing.T) {
			p, e, f := fixture(t)
			f.CreateError = apierrors.NewForbidden(resource, "run", errors.New(detail))
			_, err := e.Create(t.Context(), p)
			if err == nil || publicerr.Permanent(err) || publicerr.Classify(err).Code() != publicerr.Unavailable || f.Gets != 1 {
				t.Fatal("temporary Forbidden made permanent", err, f.Gets)
			}
		})
	}
}
