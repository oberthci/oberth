package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	fakerest "k8s.io/client-go/rest/fake"
)

func TestExecutionTargetPolicyDisabledAndClosed(t *testing.T) {
	if policy, err := readArgoTargetPolicy(""); err != nil || policy != nil {
		t.Fatal(policy, err)
	}
	if _, err := readArgoTargetPolicy("relative.json"); err == nil {
		t.Fatal("relative policy path accepted")
	}
	if err := validateArgoServeOptions(serveOptions{argoExecutionTargetConfig: "/etc/oberth/target.json"}); err == nil {
		t.Fatal("target without native Argo engine accepted")
	}
	for _, body := range []string{`{}`, `null`, `{"schema":"oberth.execution-target/v1","schema":"other"}`, `{"unknown":true}`, `{"schema":"oberth.execution-target/v1","config":{"Namespace":"a","Namespace":"b"}}`, `{} {}`} {
		name := filepath.Join(t.TempDir(), "target.json")
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if policy, err := readArgoTargetPolicy(name); err == nil || policy != nil {
			t.Fatal("incomplete/duplicate policy accepted", body)
		}
	}
}

func TestExecutionTargetTransportNeverRetriesJournaledPOST(t *testing.T) {
	for _, kind := range []string{"Workflow", "Pod", "PersistentVolumeClaim"} {
		t.Run(kind, func(t *testing.T) {
			posts := 0
			client := &fakerest.RESTClient{GroupVersion: corev1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs.WithoutConversion(), Client: fakerest.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
				posts++
				if request.Method != "POST" {
					t.Fatal("unexpected transport method")
				}
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"0"}, "Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"TooManyRequests","message":"retry requested","code":429}`))}, nil
			})}
			var err error
			switch kind {
			case "Workflow":
				_, err = (&singlePostWorkflows{client: client, namespace: "remote"}).Create(context.Background(), &wfv1.Workflow{}, metav1.CreateOptions{})
			case "Pod":
				_, err = (&singlePostPods{client: client, namespace: "remote"}).Create(context.Background(), &corev1.Pod{}, metav1.CreateOptions{})
			case "PersistentVolumeClaim":
				_, err = (&singlePostClaims{client: client, namespace: "remote"}).Create(context.Background(), &corev1.PersistentVolumeClaim{}, metav1.CreateOptions{})
			}
			if err == nil || posts != 1 {
				t.Fatal("hidden POST retry", posts, err)
			}
		})
	}
}
