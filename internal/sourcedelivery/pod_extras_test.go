package sourcedelivery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/sourcedelivery"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodExtrasAndLiveNamespaceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		names, uids   authv1.ExtraValue
		liveNamespace string
		wantReads     int
	}{
		{"multiple Pod names", authv1.ExtraValue{"pod", "other"}, authv1.ExtraValue{"uid"}, "runs", 0},
		{"multiple Pod UIDs", authv1.ExtraValue{"pod"}, authv1.ExtraValue{"uid", "other"}, "runs", 0},
		{"empty Pod UID", authv1.ExtraValue{"pod"}, authv1.ExtraValue{""}, "runs", 0},
		{"empty Pod name", authv1.ExtraValue{""}, authv1.ExtraValue{"uid"}, "runs", 0},
		{"wrong live namespace", authv1.ExtraValue{"pod"}, authv1.ExtraValue{"uid"}, "other", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			auth := sourcedelivery.KubernetesAuth{
				Review: func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error) {
					return &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{
						Username: "system:serviceaccount:runs:pipeline",
						Extra:    map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": tc.names, "authentication.kubernetes.io/pod-uid": tc.uids},
					}}}, nil
				},
				Pod: func(_ context.Context, namespace, name string) (*corev1.Pod, error) {
					reads++
					if namespace != "runs" || name != "pod" {
						t.Error("Pod lookup did not use the reviewed identity")
					}
					return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: tc.liveNamespace, Name: "pod", UID: "uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "wf", UID: "wf-uid", Controller: new(true)}}}, Spec: corev1.PodSpec{ServiceAccountName: "pipeline"}}, nil
				},
			}
			_, err := auth.Authenticate(t.Context(), "bound")
			var failure *sourcedelivery.Failure
			if !errors.As(err, &failure) || failure.Code != "forbidden" || reads != tc.wantReads {
				t.Fatalf("Pod identity was not refused at the expected boundary: %v, reads %d", err, reads)
			}
		})
	}
}
