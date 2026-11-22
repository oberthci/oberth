package argojob

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The busy remedy ("push the same commit again") is for an API that gave no
// answer or answered 429, 500, 503 or 504; every other answer is one a second
// push cannot change, so it keeps the account remedy.
func TestAccountProblemCallsTheAPIBusyOnlyForTransientAnswers(t *testing.T) {
	t.Parallel()
	accounts := schema.GroupResource{Resource: "serviceaccounts"}
	for _, tc := range []struct {
		name  string
		cause error
		busy  bool
	}{
		{"no API answer", errors.New("client rate limiter Wait returned an error"), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"internal error", apierrors.NewInternalError(errors.New("etcd")), true},
		{"service unavailable", apierrors.NewServiceUnavailable("restarting"), true},
		{"gateway timeout", apierrors.NewTimeoutError("took too long", 1), true},
		{"not found", apierrors.NewNotFound(accounts, "ci"), false},
		{"forbidden", apierrors.NewForbidden(accounts, "ci", errors.New("no create")), false},
		{"unauthorized", apierrors.NewUnauthorized("token expired"), false},
		{"denied by a webhook", apierrors.NewBadRequest("denied"), false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "ServiceAccount"}, "ci", nil), false},
		{"method not allowed", apierrors.NewMethodNotSupported(accounts, "create"), false},
		{"no UID", &accountUIDError{name: "ci"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := accountProblem(true, fmt.Errorf("account ci: %w", tc.cause))
			var public *publicerr.Error
			if !errors.As(err, &public) {
				t.Fatal("no public refusal", err)
			}
			if busy := public.Remedy() == busyRemedy; busy != tc.busy {
				t.Fatalf("busy %v, want %v: %s %s", busy, tc.busy, public.Reason(), public.Remedy())
			}
			if !tc.busy && !strings.Contains(public.Remedy(), "Install the admitted step account") {
				t.Fatal("a permanent answer lost the account remedy", public.Remedy())
			}
		})
	}
}
