package installer_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestNativeReleasePayloadInventoryIsComplete(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"fresh", "valid", "empty all", "corrupt all", "empty older", "corrupt latest"} {
		t.Run(shape, func(t *testing.T) { releaseInventoryCase(t, shape) })
	}
}

func releaseInventoryCase(t *testing.T, shape string) {
	t.Helper()
	items := releaseInventoryFixture(t)
	changeReleasePayloads(items, shape)
	cfg, reads := releaseInventoryConfig(items)
	port := installer.NewChartLibrary(kubeconfig.Target{Config: cfg}, 0)
	history, err := port.Read(t.Context(), "oberth")
	if shape == "fresh" || shape == "valid" {
		want := 3
		if shape == "fresh" {
			want = 0
		}
		if err != nil || history.Latest.Number != want {
			t.Fatal("valid history refused", history, err)
		}
	} else {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "release-provenance" {
			t.Fatal("incomplete history did not refuse provenance", history, err)
		}
	}
	if *reads != 1 {
		t.Fatal("inventory and decoding used different reads", *reads)
	}
}

func releaseInventoryFixture(t *testing.T) *corev1.SecretList {
	t.Helper()
	_, source, _ := pendingFixture(t, true)
	secrets := clientfake.NewClientset().CoreV1().Secrets("oberth")
	store := storage.Init(driver.NewSecrets(secrets))
	for number := 1; number <= 3; number++ {
		record, err := source.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	items, err := secrets.List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func changeReleasePayloads(items *corev1.SecretList, shape string) {
	if shape == "fresh" {
		items.Items = nil
	}
	for i := range items.Items {
		if strings.HasSuffix(shape, "all") || shape == "empty older" && i == 0 || shape == "corrupt latest" && i == len(items.Items)-1 {
			items.Items[i].Data["release"] = nil
			if strings.HasPrefix(shape, "corrupt") {
				items.Items[i].Data["release"] = []byte("invalid")
			}
		}
	}
}

func releaseInventoryConfig(items *corev1.SecretList) (*rest.Config, *int) {
	reads := new(int)
	cfg := &rest.Config{Host: "https://offline.invalid", Transport: deltaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && r.URL.Path == "/version" {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)), Request: r}, nil
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/oberth/secrets" || r.URL.Query().Get("labelSelector") != "name=oberth,owner=helm" {
			return nil, fmt.Errorf("unexpected history request: %s %s", r.Method, r.URL)
		}
		*reads++
		raw, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})}
	return cfg, reads
}
