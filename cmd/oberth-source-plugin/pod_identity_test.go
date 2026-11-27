package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sourceplugin"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

type sourceIdentity struct{ reviews, stages int }

// TokenReview owns audience and expiry; an unauthenticated review refuses.
// These responses also exercise authenticated Pod extras.
func (s *sourceIdentity) review(t *testing.T, r *authv1.TokenReview) *authv1.TokenReview {
	t.Helper()
	s.reviews++
	if len(r.Spec.Audiences) != 0 {
		t.Fatal("review must use the projection's default API server audience")
	}
	namespace, name, uid := "runs", "pod", "pod-uid"
	status := authv1.TokenReviewStatus{Authenticated: true}
	switch r.Spec.Token {
	case "bound-token", "rotated-token":
	case "foreign-pod":
		name, uid = "other", "other-uid"
	case "replaced-pod":
		uid = "old-uid"
	case "foreign-namespace":
		namespace = "other"
	case "review-error":
		status.Error = "verification failed"
	default:
		status.Authenticated = false
	}
	status.User = authv1.UserInfo{
		Username: "system:serviceaccount:" + namespace + ":pipeline",
		Extra:    map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {name}, "authentication.kubernetes.io/pod-uid": {uid}},
	}
	return &authv1.TokenReview{Status: status}
}

func (s *sourceIdentity) handler(t *testing.T) http.Handler {
	t.Helper()
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	p := prepared(t, f.Binding())
	auth := sourcedelivery.KubernetesAuth{
		Review: func(_ context.Context, r *authv1.TokenReview) (*authv1.TokenReview, error) {
			return s.review(t, r), nil
		},
		Pod: func(_ context.Context, namespace, name string) (*corev1.Pod, error) {
			workflow, uid := "wf", "uid"
			if name == "other" {
				workflow, uid = "other-workflow", "other-workflow-uid"
			}
			return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid"),
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow, UID: types.UID(uid), Controller: new(true)}},
			}, Spec: corev1.PodSpec{ServiceAccountName: "pipeline"}}, nil
		},
	}
	return sourcedelivery.New(sourcedelivery.Config{Now: clock.RealClock{}.Now, Budgets: &sourcedelivery.Budgets{Exports: 1, Archives: 2, Transfers: 2, Auth: 10, WorkflowWaiters: 16},
		Authenticate: auth.Authenticate,
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Admission: p.View(), Namespace: "runs", Workflow: "wf", UID: "uid"}, nil
		},
		Stage: func(ctx context.Context, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
			s.stages++
			return cache.Stage(ctx, b)
		},
	})
}

func TestPodTokenFileAgainstRealSourceHandler(t *testing.T) {
	s := &sourceIdentity{}
	handler := s.handler(t)
	for _, token := range []string{"bound-token", "unauthenticated", "foreign-pod", "replaced-pod", "foreign-namespace", "review-error", "missing"} {
		t.Run(token, func(t *testing.T) {
			cfg := pluginConfig(t, handler)
			client, err := sourceplugin.New(cfg)
			storeOK(t, err)
			defer client.Close()
			s.checkLoad(t, client, cfg.TokenFile, token)
			if token == "bound-token" {
				s.checkLoad(t, client, cfg.TokenFile, "rotated-token")
			}
		})
	}
}

func (s *sourceIdentity) checkLoad(t *testing.T, client *sourceplugin.Client, tokenFile, token string) {
	t.Helper()
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: `{"version":1,"kind":"main"}`}}
	storeOK(t, os.WriteFile(tokenFile, []byte(token), 0600))
	if token == "missing" {
		storeOK(t, os.Remove(tokenFile))
	}
	wantReviews, beforeStages := s.reviews+1, s.stages
	if token == "missing" {
		wantReviews--
	}
	path := filepath.Join(t.TempDir(), "source.tgz")
	err := client.Load(t.Context(), input, path)
	if s.reviews != wantReviews {
		t.Fatal("load did not use the expected native token review")
	}
	if token == "bound-token" || token == "rotated-token" {
		storeOK(t, err)
		checkSourceArchive(t, path)
		if s.stages != beforeStages+1 {
			t.Fatal("load bypassed native authentication or source staging")
		}
		return
	}
	if err == nil || s.stages != beforeStages {
		t.Fatal("invalid Pod identity reached source staging")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("failed load published an archive")
	}
	if token == "missing" && !strings.Contains(err.Error(), tokenFile) {
		t.Fatal("missing token must name the path without making a request")
	}
}

func TestPodTokenRotationDuringSourceRetry(t *testing.T) {
	s := &sourceIdentity{}
	delivery := s.handler(t)
	calls := 0
	cfg := pluginConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer rotated-token" {
			t.Error("retry reused the old Pod token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		delivery.ServeHTTP(w, r)
	}))
	cfg.Wait = func(context.Context, time.Duration) error {
		return os.WriteFile(cfg.TokenFile, []byte("rotated-token"), 0600)
	}
	client, err := sourceplugin.New(cfg)
	storeOK(t, err)
	defer client.Close()
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: `{"version":1,"kind":"main"}`}}
	path := filepath.Join(t.TempDir(), "source.tgz")
	storeOK(t, client.Load(t.Context(), input, path))
	checkSourceArchive(t, path)
	if calls != 2 || s.reviews != 1 || s.stages != 1 {
		t.Fatal("retry did not deliver through the real source handler")
	}
}
