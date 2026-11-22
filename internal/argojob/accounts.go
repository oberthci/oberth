package argojob

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/admissioncontract"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EnsureAccount creates a missing admitted account; an existing one costs one read.
func (e *Executor) EnsureAccount(ctx context.Context, name string) error {
	_, err := e.config.Accounts.Get(ctx, name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		automount := false
		account := &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "oberth"}}, AutomountServiceAccountToken: &automount}
		_, err = e.config.Accounts.Create(ctx, account, meta.CreateOptions{})
	}
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return accountProblem(false, fmt.Errorf("account %s: %w", name, err))
}

// verifyAccount reads an account's UID once per admission.
func (e admission) verifyAccount(ctx context.Context, name string) (admissioncontract.AccountEvidence, error) {
	if evidence, read := e.accounts[name]; read {
		return evidence, nil
	}
	a, err := e.config.Accounts.Get(ctx, name, meta.GetOptions{})
	if err != nil {
		return admissioncontract.AccountEvidence{}, fmt.Errorf("account %s: %w", name, err)
	}
	if a.Name != name || a.UID == "" {
		return admissioncontract.AccountEvidence{}, &accountUIDError{name: name}
	}
	evidence := admissioncontract.AccountEvidence{Name: a.Name, UID: string(a.UID)}
	e.accounts[name] = evidence
	return evidence, nil
}
