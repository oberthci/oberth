package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argowatch"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func startupRecoveryWatchHub(t *testing.T, namespace string) *argowatch.Hub {
	t.Helper()
	argo := argofake.NewSimpleClientset()
	argo.PrependWatchReactor("workflows", func(action clienttesting.Action) (bool, watch.Interface, error) {
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming lists unsupported")
		}
		return false, nil, nil
	})
	hub, err := argowatch.New(kubefake.NewClientset(), argo, namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	wait, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := hub.WaitForSync(wait); err != nil {
		t.Fatal(err)
	}
	return hub
}

func TestArgoStartupCompletedAdoptionUsesResumedWait(t *testing.T) {
	controller := &adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "spec-identity"}}
	collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
	run := model.Run{ID: "run", WorkflowName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"}
	for range 2 {
		if found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
			t.Fatal(found, err)
		}
	}
	if collector.calls != 0 || controller.resumed {
		t.Fatal("adoption performed terminal effects")
	}
	result, err := jobs.WaitResumed(t.Context(), run.WorkflowName, io.Discard, nil, 0)
	if err != nil || result.Status != model.RunPassed || len(jobs.intents) != 0 || collector.calls != 1 || string(artifacts.stored) != "archive" || !controller.resumed {
		t.Fatal(result, err, jobs.intents, collector.calls)
	}
}

func TestArgoStartupWatchUnavailableKeepsRetryClass(t *testing.T) {
	controller := &adoptingArgoControl{adoptErr: fmt.Errorf("watch: %w", argojob.ErrAdoptionUnavailable)}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}}
	found, err := jobs.Adopt(t.Context(), model.Run{ID: "run", WorkflowName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"})
	if found || !errors.Is(err, service.ErrUnavailable) || !errors.Is(err, argojob.ErrAdoptionUnavailable) || errors.Is(err, service.ErrAdoptionRefused) {
		t.Fatal(found, err)
	}
}
