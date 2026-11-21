package installer_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	urlpath "path"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	installfake "github.com/oberthci/oberth/internal/installer/fake"
	definitionclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func typedDefinitionReader(t *testing.T, fixture *installfake.Definitions) installer.DefinitionReader {
	t.Helper()
	reads := 0
	cfg := &rest.Config{Host: "https://offline.invalid", Transport: deltaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/") {
			t.Fatal("unexpected definition request", r.Method, r.URL.Path)
		}
		reads++
		var object any = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404}
		code := http.StatusNotFound
		if fixture != nil && fixture.Objects[urlpath.Base(r.URL.Path)] != nil {
			object, code = fixture.Objects[urlpath.Base(r.URL.Path)], http.StatusOK
		}
		raw, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	})}
	client, err := definitionclient.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if reads != 8 {
			t.Error("typed reader did not inspect all definitions", reads)
		}
	})
	return client.ApiextensionsV1().CustomResourceDefinitions()
}
