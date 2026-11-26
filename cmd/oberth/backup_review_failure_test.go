package main

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	auth "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestBackupReviewDistinguishesInvalidBearerFromAPIFailure(t *testing.T) {
	for _, row := range []struct {
		name    string
		failure error
		code    publicerr.Code
		calls   int
	}{
		{"invalid-bearer", nil, publicerr.Forbidden, 1},
		{"api-failure", errors.New("private API failure"), publicerr.Unavailable, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			calls := 0
			kube := kfake.NewClientset()
			kube.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				calls++
				return true, &auth.TokenReview{Status: auth.TokenReviewStatus{Error: "invalid bearer token"}}, row.failure
			})
			now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
			owner := &serverOwners{c: &composition{config: serverConfig{Namespace: "oberth"}, clock: serverClock{Now: func() time.Time { return now }}}, cluster: clusterClients{backupReview: kube}}
			snapshot := newServerBackupSnapshot(owner)
			_, err := snapshot.Snapshot(t.Context(), "10.0.0.1:1234", "junk")
			if e := publicerr.Classify(err); err == nil || e.Code() != row.code {
				t.Fatal("wrong first refusal", err)
			}
			_, err = snapshot.Snapshot(t.Context(), "10.0.0.1:5678", "junk")
			if e := publicerr.Classify(err); err == nil || e.Code() != publicerr.Unavailable {
				t.Fatal("wrong second refusal", err)
			}
			if calls != row.calls {
				t.Fatal("cooldown did not distinguish an invalid bearer from an API outage", calls)
			}
			now = now.Add(deliveryCooldown)
			_, _ = snapshot.Snapshot(t.Context(), "10.0.0.1:9012", "junk")
			if calls != row.calls+1 {
				t.Fatal("cooldown did not expire", calls)
			}
		})
	}
}
