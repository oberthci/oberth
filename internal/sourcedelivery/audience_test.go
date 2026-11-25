package sourcedelivery_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/sourcedelivery"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
)

func TestStoreAudienceMustBeReturnedByTokenReview(t *testing.T) {
	for _, audiences := range [][]string{nil, {"api"}} {
		k := sourcedelivery.KubernetesAuth{Audience: "store", Review: func(_ context.Context, r *auth.TokenReview) (*auth.TokenReview, error) {
			if len(r.Spec.Audiences) != 1 || r.Spec.Audiences[0] != "store" {
				t.Fatal("store audience not requested")
			}
			return &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: true, Audiences: audiences, User: auth.UserInfo{Username: "system:serviceaccount:runs:step", Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {"pod"}, "authentication.kubernetes.io/pod-uid": {"uid"}}}}}, nil
		}, Pod: func(context.Context, string, string) (*core.Pod, error) {
			t.Error("wrong audience reached live Pod read")
			return nil, nil
		}}
		if _, err := k.Authenticate(t.Context(), "fixture"); err == nil {
			t.Fatal("TokenReview authenticated the wrong audience")
		}
	}
}
