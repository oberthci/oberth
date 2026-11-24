package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argoclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
)

type finalizerAPI struct {
	t              *testing.T
	patches, pages atomic.Int64
}

func (f *finalizerAPI) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	out.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cronworkflows"):
		_ = json.NewEncoder(out).Encode(wf.CronWorkflowList{})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workflows"):
		f.list(out, r)
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/workflows/"):
		f.patch(out, r)
	default:
		f.t.Error("unexpected uninstall API call", r.Method, r.URL.Path)
		http.Error(out, "unexpected", http.StatusBadRequest)
	}
}

func (f *finalizerAPI) list(out http.ResponseWriter, r *http.Request) {
	f.pages.Add(1)
	if r.URL.Query().Get("limit") != "200" || !strings.Contains(r.URL.Query().Get("labelSelector"), "controller-instanceid=frontdoor") {
		f.t.Error("uninstall lost its page or installation scope", r.URL)
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("continue"))
	page := wf.WorkflowList{}
	for i := offset; i < min(offset+200, 500); i++ {
		name := fmt.Sprintf("workflow-%03d", i)
		page.Items = append(page.Items, wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "jobs", UID: types.UID(name), ResourceVersion: "1", Labels: map[string]string{"oberth.ci/engine": "argo", "workflows.argoproj.io/controller-instanceid": "frontdoor"}, Finalizers: []string{"oberth.ci/run-record", "other.ci/guard"}}})
	}
	if offset+200 < 500 {
		page.Continue = strconv.Itoa(offset + 200)
	}
	_ = json.NewEncoder(out).Encode(page)
}

func (f *finalizerAPI) patch(out http.ResponseWriter, r *http.Request) {
	var patch []struct {
		Op, Path string
		Value    json.RawMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || len(patch) != 3 || string(patch[2].Value) != `["other.ci/guard"]` {
		f.t.Error("uninstall removed another finalizer", patch, err)
	}
	f.patches.Add(1)
	_ = json.NewEncoder(out).Encode(wf.Workflow{})
}

func TestUninstallFinalizersDrainFiveHundredWorkflows(t *testing.T) {
	t.Parallel()
	api := &finalizerAPI{t: t}
	server := httptest.NewServer(api)
	defer server.Close()
	config := finalizerClientConfig(&rest.Config{Host: server.URL})
	client, err := argoclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	wall := clock.RealClock{}
	start := wall.Now()
	err = installer.ReleaseRunFinalizers(ctx, client, installer.FinalizerScope{Namespace: "jobs", Instance: "frontdoor"})
	if err != nil || api.patches.Load() != 500 || api.pages.Load() != 3 {
		t.Fatal("uninstall cannot drain 500 guarded Workflows within its budget", api.patches.Load(), api.pages.Load(), wall.Since(start), err)
	}
	t.Log("500 UID/RV-fenced patches over 3 pages:", wall.Since(start), "QPS", config.QPS, "burst", config.Burst)
}
