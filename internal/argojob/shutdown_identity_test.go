package argojob

import (
	"context"
	"io"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestNativeShutdownIdentityDelivery(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{"", wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			controller, wf, pod, requests := executorRetryFixture(t)
			wf.Spec.Shutdown = strategy
			controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return nil
			}
			progress := &executorIdentityProgress{identity: map[types.UID]identityWait{pod.UID: {since: time.Now().Add(-2 * executorReadinessTimeout), retried: true, persisted: true}}}
			delivered := map[types.UID]bool{}
			err := controller.observeWorkflowIdentities(t.Context(), wf, []corev1.Pod{*pod}, progress, delivered)
			if strategy == "" {
				if err == nil || *requests != 0 {
					t.Fatal("expired ordinary startup escaped its deadline", err, *requests)
				}
				return
			}
			want := 0
			if strategy == wfv1.ShutdownStrategyStop {
				want = 1
			}
			if err != nil || *requests != want || delivered[pod.UID] != (want == 1) {
				t.Fatalf("shutdown %q token requests=%d delivered=%t error=%v", strategy, *requests, delivered[pod.UID], err)
			}
		})
	}
}
