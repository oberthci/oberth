package main

import (
	"context"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type serverBackupSnapshot struct{ owner *serverOwners }

func backupAccountName(namespace string) string {
	return "system:serviceaccount:" + namespace + ":" + secretstore.BackupRole
}

func (s serverBackupSnapshot) Snapshot(ctx context.Context, token string) (io.ReadCloser, error) {
	a, err := s.authenticatedAccount(ctx, token)
	if err != nil {
		return nil, err
	}
	if err = s.owner.service.CheckBackupAccount(ctx, a); err != nil {
		return nil, err
	}
	return backupStream(ctx, func(ctx context.Context, out io.WriteCloser) error {
		_, err := s.owner.service.BackupAccount(ctx, a, out)
		return err
	}), nil
}

func (s serverBackupSnapshot) authenticatedAccount(ctx context.Context, token string) (actor.Actor, error) {
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	review, err := s.owner.cluster.Kube.AuthenticationV1().TokenReviews().Create(check,
		&authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{apicontract.BackupSnapshotAudience}}}, metav1.CreateOptions{})
	account := backupAccountName(s.owner.c.config.Namespace)
	if err != nil || review == nil || review.Status.Error != "" {
		return actor.Actor{}, publicerr.New(publicerr.Unavailable, "Backup authorization is unavailable.", "Check the Kubernetes API and backup account; retry this backup explicitly.", nil)
	}
	if !review.Status.Authenticated || review.Status.User.Username != account || review.Status.User.UID == "" || len(review.Status.Audiences) != 1 || review.Status.Audiences[0] != apicontract.BackupSnapshotAudience {
		return actor.Actor{}, publicerr.New(publicerr.Forbidden, "Backup account was not authorized.", "Run the backup Job with its scoped ServiceAccount in the server namespace.", nil)
	}
	return actor.Actor{Identity: account, Fingerprint: review.Status.User.UID, AuthMethod: "service-account"}, nil
}
