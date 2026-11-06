package argojob

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func nonrootFixtureClient(objects ...runtime.Object) *fake.Clientset {
	pod, cm := nonrootVerifierObjects()
	return fake.NewClientset(append([]runtime.Object{pod, cm}, objects...)...)
}
func prepareNonrootFixture(t *testing.T, config Config, request Request) Request {
	t.Helper()
	request.nonrootProof = nil
	controller, err := NewController(&staticWorkflowClient{}, nonrootFixtureClient(), config)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := controller.PrepareNonroot(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}
