package sourcedelivery_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutorTokenFetchesOnlyItsOwnRunsSource(t *testing.T) {
	for _, tc := range []sourceScopeCase{
		{"own", "ci", "run-0001", "workflow-uid", false, http.StatusOK},
		{"another run", "ci", "run-0002", "another-uid", false, http.StatusForbidden},
		{"recreated Workflow", "ci", "run-0001", "another-uid", false, http.StatusForbidden},
		{"another namespace", "other", "run-0001", "workflow-uid", false, http.StatusForbidden},
		{"terminal run", "ci", "run-0001", "workflow-uid", true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSourceTokenScope(t, tc)
		})
	}
}

type sourceScopeCase struct {
	name, namespace, workflow, uid string
	terminal                       bool
	status                         int
}

func executorPodTokenAuth(t *testing.T) (sourcedelivery.KubernetesAuth, *int) {
	t.Helper()
	controller := true
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "ci", Name: "run-0001-build-123", UID: "pod-uid", OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "run-0001", UID: "workflow-uid", Controller: &controller}}}, Spec: core.PodSpec{ServiceAccountName: "pipeline"}}
	reviews := 0
	authn := sourcedelivery.KubernetesAuth{Review: func(_ context.Context, r *auth.TokenReview) (*auth.TokenReview, error) {
		if r.Spec.Token != "synthetic-executor-token" || len(r.Spec.Audiences) != 0 {
			t.Fatal("expected the executor's API-audience token")
		}
		reviews++
		return &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: true, User: auth.UserInfo{Username: "system:serviceaccount:ci:pipeline", Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}}, nil
	}, Pod: func(_ context.Context, namespace, name string) (*core.Pod, error) {
		if namespace != pod.Namespace || name != pod.Name {
			t.Fatal("wrong Pod lookup")
		}
		return pod, nil
	}}

	return authn, &reviews
}

func assertSourceTokenScope(t *testing.T, tc sourceScopeCase) {
	t.Helper()
	authn, reviews := executorPodTokenAuth(t)
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	staged := 0
	server := sourcedelivery.New(sourcedelivery.Config{Now: func() time.Time { return time.Unix(1, 0) }, Authenticate: authn.Authenticate,
		Run: func(_ context.Context, locator string) (sourcedelivery.Run, error) {
			if locator != "requested-run" {
				t.Fatal("wrong run lookup")
			}
			return sourcedelivery.Run{Namespace: tc.namespace, Workflow: tc.workflow, UID: tc.uid, Terminal: tc.terminal, Admission: admissioncontract.PreparedView{RunID: locator, Digest: strings.Repeat("a", 64), SourceBindings: []sourcecontract.Binding{{Descriptor: descriptor}}}}, nil
		}, Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			staged++
			return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
		},
	})
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: "requested-run", Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer synthetic-executor-token")
	response := httptest.NewRecorder()
	before := *reviews
	server.ServeHTTP(response, request)
	wantStaged := 0
	if tc.status == http.StatusOK {
		wantStaged = 1
	}
	if response.Code != tc.status || staged != wantStaged || *reviews != before+1 {
		t.Fatalf("status=%d staged=%d reviews=%d", response.Code, staged, *reviews-before)
	}
	if tc.status == http.StatusOK && response.Header().Get(sourcedelivery.ReceiptTrailer) == "" {
		t.Fatal("own source has no receipt")
	}
}
