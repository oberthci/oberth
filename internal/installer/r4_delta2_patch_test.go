package installer_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

func TestDelta2HistoricalMergeBaseChangesFieldsOnlyOnTarget(t *testing.T) {
	t.Parallel()
	live := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "verified-target", Namespace: "oberth"}, Data: map[string]string{"keep": "live", "removed-by-history": "live"}}
	var patches []string
	var body []byte
	cfg := &rest.Config{Host: "https://offline.invalid", ContentConfig: rest.ContentConfig{GroupVersion: &schema.GroupVersion{Version: "v1"}, NegotiatedSerializer: scheme.Codecs.WithoutConversion()}, APIPath: "/api", Transport: deltaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/namespaces/oberth/configmaps/verified-target" {
			return nil, fmt.Errorf("history redirected request to %s", r.URL.Path)
		}
		if r.Method == "PATCH" {
			patches = append(patches, r.URL.Path)
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
		} else if r.Method != "GET" {
			return nil, fmt.Errorf("unexpected request method %s", r.Method)
		}
		raw, err := json.Marshal(live)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})}
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mapping := &meta.RESTMapping{Resource: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Scope: meta.RESTScopeNamespace}
	old := live.DeepCopy()
	old.Data = map[string]string{"keep": "old", "removed-by-history": "invented history"}
	target := live.DeepCopy()
	target.Data = map[string]string{"keep": "verified"}
	original := kube.ResourceList{&resource.Info{Client: client, Mapping: mapping, Namespace: "oberth", Name: "verified-target", Object: old}}
	desired := kube.ResourceList{&resource.Info{Client: client, Mapping: mapping, Namespace: "oberth", Name: "verified-target", Object: target}}
	if _, err = kube.New(nil).Update(original, desired, false); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || !strings.Contains(string(body), `"removed-by-history":null`) {
		t.Fatalf("probe did not demonstrate history field authority: paths=%v patch=%s", patches, body)
	}
	t.Logf("native merge patch stayed on target: %s patch=%s", patches[0], body)
}
