package secretstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliveryPublishesOnlyACompletePrivateSet(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		deliveryCase(t, conflict)

	}
}

func deliveryCase(t *testing.T, conflict bool) {
	t.Helper()
	parent := t.TempDir()
	ran := false
	c, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		var result any = map[string]any{"data": map[string]string{"value": base64.StdEncoding.EncodeToString([]byte("test-value"))}}
		if strings.HasSuffix(r.URL.Path, "/login") {
			result = map[string]any{"auth": map[string]string{"client_token": "session"}}
		}
		raw, _ := json.Marshal(result)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"one", "two"}
	if conflict {
		paths = []string{"one", "one/nested"}
	}
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	cfg := Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "oberth-runs", ServerAccount: "oberth", Audience: "oberth-store"}
	err = c.deliver(t.Context(), cfg, Delivery{Parent: parent, Token: []byte("header." + body + ".signature"), Paths: paths, Run: func(_ context.Context, dir string) error {
		ran = true
		for _, name := range paths {
			st, e := os.Stat(filepath.Join(dir, name))
			if e != nil || st.Mode().Perm() != 0600 {
				t.Fatal("incomplete/public set", e)
			}
		}
		privateDirectory(t, dir)
		return nil
	}}, func(string) error { return nil })
	if (err == nil) == conflict || ran == conflict {
		t.Fatal("delivery publication", conflict, err)
	}
	files, e := os.ReadDir(parent)
	if e != nil || len(files) != 0 {
		t.Fatal("partial secrets retained", e)
	}
}

func privateDirectory(t *testing.T, dir string) {
	t.Helper()
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("public directory", err)
	}
}

func TestPublicDeliveryRequiresUsableParentBeforeFetching(t *testing.T) {
	called := false
	c, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	})})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "oberth-runs", ServerAccount: "oberth", Audience: "oberth-store"}
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	err = c.Deliver(t.Context(), cfg, Delivery{Parent: filepath.Join(t.TempDir(), "absent"), Token: []byte("header." + body + ".signature"), Paths: []string{"one"}, Run: func(context.Context, string) error { t.Fatal("child ran without a usable parent"); return nil }})
	if err == nil || called {
		t.Fatal("delivery did not check its parent before reading secrets", err)
	}
}
