package main

import (
	"context"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The snapshot endpoint is public, so its TokenReview never draws on the main
// client's budget: it has its own small one, at most two reviews in flight and
// one per remote address. An address that presented an invalid token is cooled
// as broker addresses are. Overflow and cooldown answer a retryable 503 without
// an API call. A backup Job reviews once per run, far inside this budget.
const (
	backupReviewQPS, backupReviewBurst = 1, 2
	backupReviewsInFlight              = 2
)

type serverBackupSnapshot struct {
	owner *serverOwners
	gate  *deliveryAuthGate
}

func newServerBackupSnapshot(o *serverOwners) serverBackupSnapshot {
	return serverBackupSnapshot{owner: o, gate: newAuthGate(backupReviewsInFlight, o.c.clock.Now)}
}

func backupAccountName(namespace string) string {
	return "system:serviceaccount:" + namespace + ":" + secretstore.BackupRole
}

func (s serverBackupSnapshot) Snapshot(ctx context.Context, remote, token string) (io.ReadCloser, error) {
	a, err := s.boundedAccount(ctx, addressHost(remote), token)
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

func (s serverBackupSnapshot) boundedAccount(ctx context.Context, remote, token string) (a actor.Actor, err error) {
	pod := ""
	if s.owner.podAt != nil {
		pod = s.owner.podAt(remote)
	}
	if s.gate.acquire(remote, pod) != nil {
		return actor.Actor{}, publicerr.New(publicerr.Unavailable, "Backup authorization is busy.", "Retry this backup in a few seconds.", nil)
	}
	defer func() {
		outcome := err
		if err != nil && publicerr.Classify(err).Code() == publicerr.Forbidden {
			outcome = &sourcedelivery.Failure{Code: "unauthenticated"}
		}
		s.gate.release(remote, pod, outcome)
	}()
	return s.authenticatedAccount(ctx, token)
}

func (s serverBackupSnapshot) authenticatedAccount(ctx context.Context, token string) (actor.Actor, error) {
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	review, err := s.owner.cluster.backupReview.AuthenticationV1().TokenReviews().Create(check,
		&authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{apicontract.BackupSnapshotAudience}}}, metav1.CreateOptions{})
	account := backupAccountName(s.owner.c.config.Namespace)
	if err != nil || review == nil || review.Status.Authenticated && review.Status.Error != "" {
		return actor.Actor{}, publicerr.New(publicerr.Unavailable, "Backup authorization is unavailable.", "Check the Kubernetes API and backup account; retry this backup explicitly.", nil)
	}
	if !review.Status.Authenticated || review.Status.User.Username != account || review.Status.User.UID == "" || len(review.Status.Audiences) != 1 || review.Status.Audiences[0] != apicontract.BackupSnapshotAudience {
		return actor.Actor{}, publicerr.New(publicerr.Forbidden, "Backup account was not authorized.", "Run the backup Job with its scoped ServiceAccount in the server namespace.", nil)
	}
	return actor.Actor{Identity: account, Fingerprint: review.Status.User.UID, AuthMethod: "service-account"}, nil
}
