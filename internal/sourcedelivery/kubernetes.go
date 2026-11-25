package sourcedelivery

import (
	"context"
	"slices"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubernetesAuth uses injected bounded native clients. TokenReview is the sole
// bearer authority; the bound Pod UID is checked against a live Pod before
// deriving the Workflow owner. Request selectors never establish identity.
type KubernetesAuth struct {
	Audience string
	Review   func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error)
	Pod      func(context.Context, string, string) (*corev1.Pod, error)
}

func (k KubernetesAuth) Authenticate(ctx context.Context, token string) (Caller, error) {
	if k.Review == nil || k.Pod == nil {
		return Caller{}, &Failure{Code: "unavailable", Reason: "other"}
	}
	// Both the projection and TokenReview use the API server audience by default.
	// Kubernetes verifies audience, expiry and Pod binding before returning a user.
	spec := authv1.TokenReviewSpec{Token: token}
	if k.Audience != "" {
		spec.Audiences = []string{k.Audience}
	}
	review, err := k.Review(ctx, &authv1.TokenReview{Spec: spec})
	if err != nil {
		return Caller{}, &Failure{Code: "unavailable", Reason: "other"}
	}
	if review == nil || !review.Status.Authenticated || review.Status.Error != "" {
		return Caller{}, &Failure{Code: "unauthenticated"}
	}
	if k.Audience != "" && !slices.Contains(review.Status.Audiences, k.Audience) {
		return Caller{}, &Failure{Code: "unauthenticated"}
	}
	return k.boundPod(ctx, review.Status.User)
}
func (k KubernetesAuth) boundPod(ctx context.Context, user authv1.UserInfo) (Caller, error) {
	parts := strings.Split(user.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	names := user.Extra["authentication.kubernetes.io/pod-name"]
	uids := user.Extra["authentication.kubernetes.io/pod-uid"]
	if len(names) != 1 || len(uids) != 1 {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	if names[0] == "" || uids[0] == "" {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	pod, err := k.Pod(ctx, parts[2], names[0])
	if err != nil {
		return Caller{}, &Failure{Code: "unavailable", Reason: "other"}
	}
	return podOwner(pod, parts, uids[0])
}
func podOwner(pod *corev1.Pod, parts []string, uid string) (Caller, error) {
	if pod == nil {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	if pod.Namespace != parts[2] || string(pod.UID) != uid || pod.Spec.ServiceAccountName != parts[3] || pod.DeletionTimestamp != nil {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name == "" || owner.UID == "" {
		return Caller{}, &Failure{Code: "forbidden"}
	}
	return Caller{Namespace: pod.Namespace, Workflow: owner.Name, UID: string(owner.UID), Pod: pod.DeepCopy()}, nil
}
