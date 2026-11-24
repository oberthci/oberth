package argojob_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argoclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/util/telemetry"
	"github.com/argoproj/argo-workflows/v4/workflow/gccontroller"
	"github.com/argoproj/argo-workflows/v4/workflow/metrics"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

func TestArchivedFailedWorkflowTTLCollectsPodsAndReleasesProtectedPVC(t *testing.T) {
	// The real pinned Argo TTL controller sends DELETE. Fake API reactors below
	// model Kubernetes background cascading GC and PVC protection; no cluster runs.
	db, f, b, pod := collectionFixture(t)
	f.Object.Status.Phase = wf.WorkflowFailed
	for id, n := range f.Object.Status.Nodes {
		n.Phase = wf.NodeFailed
		f.Object.Status.Nodes[id] = n
	}
	pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
	pod.Spec.Volumes = []core.Volume{{Name: "work", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "work"}}}}
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	reports := 1
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	collected := false
	native := collectionNative{Workflows: f, collected: func(w *wf.Workflow) {
		assertArchivedFailure(t, db, logs, b.RunID)
		if slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) {
			t.Error("TTL saw an unarchived Workflow")
		}
		exerciseNativeTTL(t, w, pod)
		collected = true
		cancel()
	}}
	config := argojob.Config{Runs: db, Progress: db, Logs: logs, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Reporter: collectionReporter(t, db, f, &reports), Now: compositionClock}
	_ = argojob.New(config).Run(ctx)
	if !collected {
		t.Fatal("archived run was never collected", ctx.Err())
	}
	assertArchivedFailure(t, db, logs, b.RunID)
}

func exerciseNativeTTL(t *testing.T, w *wf.Workflow, pod *core.Pod) {
	t.Helper()
	ctx, cancel := context.WithTimeout(logging.TestContext(t.Context()), 5*time.Second)
	defer cancel()
	w = w.DeepCopy()
	w.DeletionTimestamp = nil
	w.Labels["workflows.argoproj.io/completed"] = "true"
	w.Status.FinishedAt = meta.NewTime(compositionClock().Add(-48 * time.Hour))
	un, err := argoutil.ToUnstructured(w)
	compositionOK(t, err)
	events := watch.NewRaceFreeFake()
	defer events.Stop()
	informer := cache.NewSharedIndexInformer(noWatchList{&cache.ListWatch{ListFunc: func(meta.ListOptions) (runtime.Object, error) {
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*un}}, nil
	}, WatchFunc: func(meta.ListOptions) (watch.Interface, error) { return events, nil }}}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	client, deleted := nativeCollectionClient(t, ctx, w, pod)
	m, err := metrics.New(ctx, telemetry.TestScopeName, telemetry.TestScopeName, &telemetry.Config{}, metrics.Callbacks{})
	compositionOK(t, err)
	controller := gccontroller.NewController(ctx, client, informer, m, nil)
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx, 1) }()
	select {
	case <-deleted:
	case <-ctx.Done():
		t.Error("native TTL never collected the expired Workflow")
	}
	cancel()
	if err = <-done; err != nil {
		t.Error(err)
	}
}

func modelDependentCollection(t *testing.T, ctx context.Context, w *wf.Workflow, pod *core.Pod) {
	t.Helper()
	deletion := meta.Now()
	pvc := &core.PersistentVolumeClaim{ObjectMeta: meta.ObjectMeta{Name: "work", Namespace: w.Namespace, Finalizers: []string{"kubernetes.io/pvc-protection"}, DeletionTimestamp: &deletion, OwnerReferences: pod.OwnerReferences}}
	kube := kubefake.NewSimpleClientset(pod, pvc)
	claims := kube.CoreV1().PersistentVolumeClaims(w.Namespace)
	pods := kube.CoreV1().Pods(w.Namespace)
	if _, err := claims.Get(ctx, "work", meta.GetOptions{}); err != nil {
		t.Error("protected PVC disappeared while Pod referred to it", err)
	}
	owner := meta.GetControllerOf(pod)
	if owner == nil || owner.UID != w.UID {
		t.Error("Pod is not a dependent of the collected Workflow")
		return
	}
	// Model the background garbage collector's UID-fenced dependent DELETE.
	if err := pods.Delete(ctx, pod.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &pod.UID}}); err != nil {
		t.Error(err)
		return
	}
	remaining, err := pods.List(ctx, meta.ListOptions{})
	if err != nil || len(remaining.Items) != 0 {
		t.Error("collected Pod still holds its PVC", remaining, err)
		return
	}
	// Model PVC protection reconciling after the last referencing Pod disappears.
	pvc.Finalizers = nil
	if _, err = claims.Update(ctx, pvc, meta.UpdateOptions{}); err != nil {
		t.Error(err)
		return
	}
	if err = claims.Delete(ctx, pvc.Name, meta.DeleteOptions{}); err != nil {
		t.Error(err)
		return
	}
	if _, err = claims.Get(ctx, pvc.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("PVC was not released after Pod collection", err)
	}
}

// This fake watcher implements ordinary LIST/WATCH, without watch-list bookmarks.
type noWatchList struct{ *cache.ListWatch }

func (noWatchList) IsWatchListSemanticsUnSupported() bool { return true }

func nativeCollectionClient(t *testing.T, ctx context.Context, w *wf.Workflow, pod *core.Pod) (argoclient.Interface, <-chan struct{}) {
	t.Helper()
	deleted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/"+w.Namespace+"/workflows/"+w.Name {
			t.Error("unexpected native GC request", request.Method, request.URL.Path)
			out.WriteHeader(http.StatusBadRequest)
			return
		}
		var options meta.DeleteOptions
		if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
			t.Error(err)
		}
		if options.PropagationPolicy == nil || *options.PropagationPolicy != meta.DeletePropagationBackground {
			t.Error("TTL did not request background collection")
		}
		modelDependentCollection(t, ctx, w, pod)
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(meta.Status{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: meta.StatusSuccess})
		deleted <- struct{}{}
	}))
	t.Cleanup(server.Close)
	client, err := argoclient.NewForConfig(&rest.Config{Host: server.URL})
	compositionOK(t, err)
	return client, deleted
}

func assertArchivedFailure(t *testing.T, db *store.Store, logs *runlog.Store, id string) {
	t.Helper()
	record, err := db.Run(t.Context(), id)
	compositionOK(t, err)
	body, _, err := logs.Read(t.Context(), id, runlog.ReadRequest{OwnOnly: true})
	if err != nil || record.Status != stepmodel.RunFailed || !record.EvidenceComplete || string(body) != "container output\n" {
		t.Fatal("failed run or archived logs lost during native collection", record, string(body), err)
	}
}
