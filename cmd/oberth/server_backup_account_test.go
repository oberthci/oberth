package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func validBackupReview() authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{apicontract.BackupSnapshotAudience},
		User: authv1.UserInfo{Username: backupAccountName("oberth"), UID: "account-uid"}}
}

func TestBackupSnapshotRequiresExactLiveAccountAndAudience(t *testing.T) {
	for _, scenario := range []string{"valid", "ci-namespace", "other-account", "other-audience", "extra-audience", "missing-uid", "unauthenticated", "review-error"} {
		t.Run(scenario, func(t *testing.T) {
			status := backupReviewScenario(scenario)
			client := kfake.NewClientset()
			client.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
				request := action.(ktesting.CreateAction).GetObject().(*authv1.TokenReview)
				if request.Spec.Token != "projection-fixture" || !reflect.DeepEqual(request.Spec.Audiences, []string{apicontract.BackupSnapshotAudience}) {
					t.Fatal("snapshot authentication did not request its own audience")
				}
				if scenario == "review-error" {
					return true, nil, errors.New("private-review-failure")
				}
				return true, &authv1.TokenReview{Status: status}, nil
			})
			owner := &serverOwners{c: &composition{config: serverConfig{Namespace: "oberth"}}, cluster: clusterClients{Kube: client}}
			account, err := (serverBackupSnapshot{owner}).authenticatedAccount(t.Context(), "projection-fixture")
			checkBackupReviewResult(t, scenario, account, err)
		})
	}
}

func checkBackupReviewResult(t *testing.T, scenario string, account actor.Actor, err error) {
	t.Helper()
	if scenario == "valid" {
		if err != nil || account.Admin || account.AuthMethod != "service-account" || account.Identity != backupAccountName("oberth") || account.Fingerprint != "account-uid" {
			t.Fatal("live backup account was elevated or refused", account, err)
		}
	} else if err == nil || strings.Contains(err.Error(), "private-review-failure") {
		t.Fatal("invalid backup account was accepted or exposed native data", err)
	}
}

func backupReviewScenario(scenario string) authv1.TokenReviewStatus {
	status := validBackupReview()
	switch scenario {
	case "ci-namespace":
		status.User.Username = backupAccountName("oberth-runs")
	case "other-account":
		status.User.Username = "system:serviceaccount:oberth:other"
	case "other-audience":
		status.Audiences = []string{"other"}
	case "extra-audience":
		status.Audiences = append(status.Audiences, "other")
	case "missing-uid":
		status.User.UID = ""
	case "unauthenticated":
		status.Authenticated = false
	}
	return status
}

func TestBackupSnapshotReviewsAgainAfterRevocation(t *testing.T) {
	client := kfake.NewClientset()
	calls := 0
	client.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		status := validBackupReview()
		if calls > 1 {
			status.Authenticated = false
		}
		return true, &authv1.TokenReview{Status: status}, nil
	})
	owner := &serverOwners{c: &composition{config: serverConfig{Namespace: "oberth"}}, cluster: clusterClients{Kube: client}}
	snapshot := serverBackupSnapshot{owner}
	if _, err := snapshot.authenticatedAccount(t.Context(), "projection-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.authenticatedAccount(t.Context(), "projection-fixture"); err == nil || calls != 2 {
		t.Fatal("revoked backup authority was accepted from a cached read", calls, err)
	}
}
