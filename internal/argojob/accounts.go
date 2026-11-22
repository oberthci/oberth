package argojob

import (
	"context"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EnsureAccount creates a missing admitted account; an existing one costs one read.
func (e *Executor) EnsureAccount(ctx context.Context, name string) error {
	_, err := e.config.Accounts.Get(ctx, name, meta.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return err
	}
	automount := false
	account := &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "oberth"}}, AutomountServiceAccountToken: &automount}
	_, err = e.config.Accounts.Create(ctx, account, meta.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
