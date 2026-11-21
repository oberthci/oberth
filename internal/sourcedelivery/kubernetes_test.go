package sourcedelivery_test

import (
	"context"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/sourcedelivery"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPodBoundTokenUsesLiveUIDAndController(t *testing.T) {
	controller := true
	review := &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: "system:serviceaccount:runs:executor", Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "runs", Name: "pod", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "wf", UID: "wf-uid", Controller: &controller}}}, Spec: corev1.PodSpec{ServiceAccountName: "executor"}}
	outage := false
	auth := sourcedelivery.KubernetesAuth{Review: func(_ context.Context, r *authv1.TokenReview) (*authv1.TokenReview, error) {
		if r.Spec.Token != "bound" {
			t.Error("wrong token authority input")
		}
		if outage {
			return nil, io.ErrUnexpectedEOF
		}
		return review, nil
	}, Pod: func(_ context.Context, namespace, name string) (*corev1.Pod, error) {
		if namespace != "runs" || name != "pod" {
			t.Error("token did not select live Pod")
		}
		return pod, nil
	}}
	got, err := auth.Authenticate(t.Context(), "bound")
	if err != nil || got.UID != "wf-uid" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, tc := range []struct {
		name         string
		change, undo func()
	}{
		{"terminating Pod", func() { now := metav1.Now(); pod.DeletionTimestamp = &now }, func() { pod.DeletionTimestamp = nil }},
		{"wrong owner API version", func() { pod.OwnerReferences[0].APIVersion = "other/v1" }, func() { pod.OwnerReferences[0].APIVersion = "argoproj.io/v1alpha1" }},
		{"Pod replaced", func() { pod.UID = types.UID("replacement") }, func() { pod.UID = "pod-uid" }},
		{"wrong service account", func() { pod.Spec.ServiceAccountName = "other" }, func() { pod.Spec.ServiceAccountName = "executor" }},
		{"no controller", func() { controller = false }, func() { controller = true }},
		{"wrong native kind", func() { pod.OwnerReferences[0].Kind = "Other" }, func() { pod.OwnerReferences[0].Kind = "Workflow" }},
		{"unbound token", func() { delete(review.Status.User.Extra, "authentication.kubernetes.io/pod-uid") }, func() {
			review.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"pod-uid"}
		}},
		{"unauthenticated", func() { review.Status.Authenticated = false }, func() { review.Status.Authenticated = true }},
		{"review outage", func() { outage = true }, func() { outage = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			defer tc.undo()
			if _, err := auth.Authenticate(t.Context(), "bound"); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
