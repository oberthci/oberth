package installer_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"k8s.io/client-go/rest"
)

type clusterReadTransport struct {
	t     *testing.T
	reads int
}

func (r *clusterReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.reads++
	if request.Method != http.MethodGet || request.URL.Host != "selected.oberth.ci" {
		r.t.Fatal("unexpected target or write", request.Method, request.URL.Host)
	}
	if _, ok := request.Context().Deadline(); !ok {
		r.t.Fatal("chart request lost operation deadline")
	}
	body := `{"kind":"SecretList","apiVersion":"v1","items":[]}`
	if request.URL.Path == "/api/v1/namespaces/kube-system" {
		body = `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"kube-system","uid":"selected-cluster"}}`
	}
	if request.URL.Path == "/version" {
		body = `{"gitVersion":"v1.30.0","major":"1","minor":"30"}`
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestSelectedChartLibraryUsesBoundContextAndReadOnlyTransport(t *testing.T) {
	t.Parallel()
	transport := &clusterReadTransport{t: t}
	library := installer.NewChartLibrary(kubeconfig.Target{Context: "selected", Config: &rest.Config{Host: "https://selected.oberth.ci", Transport: transport}}, time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	history, err := library.Read(ctx, "oberth")
	if err != nil || history.Latest.Number != 0 || transport.reads < 2 {
		t.Fatal(history, err, transport.reads)
	}
}
func TestDefaultVerifierRefusesMissingLocalTrustWithoutNetwork(t *testing.T) {
	t.Parallel()
	if err := installer.Verify(t.Context(), installer.Profile{CAFile: t.TempDir() + "/absent"}); err == nil {
		t.Fatal("missing local trust accepted")
	}
}
