package auditanchor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSlowKubernetesReconcileDoesNotBlockWriter(t *testing.T) {
	for _, writer := range []string{"same-store", "other-store"} {
		t.Run(writer, func(t *testing.T) {
			now := time.Now()
			path := filepath.Join(t.TempDir(), "audit.sqlite")
			database, err := store.OpenAdminClient(t.Context(), path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			client := fake.NewSimpleClientset()
			kube, err := NewKubernetesContinuity(client, "oberth")
			if err != nil {
				t.Fatal(err)
			}
			continuity := &countingContinuity{Continuity: kube}
			manager, err := NewManager(ManagerConfig{Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: continuity, Interval: time.Minute, MaxAge: time.Hour, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			writeStore := database
			if writer == "other-store" {
				writeStore, err = store.OpenAdminClient(t.Context(), path, store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = writeStore.Close() }()
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var reconciling atomic.Bool
			continuity.reconcileHook = func() { reconciling.Store(true) }
			client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				if reconciling.CompareAndSwap(true, false) {
					close(entered)
					<-release
				}
				return false, nil, nil
			})
			reconcilesBefore := continuity.reconcileCalls
			gateDone := make(chan error, 1)
			go func() { gateDone <- manager.AllowMutation(t.Context()) }()
			select {
			case <-entered:
			case err := <-gateDone:
				t.Fatalf("gate did not reach Kubernetes reconciliation: %v", err)
			case <-time.After(testtimeout.Start(5 * time.Second)):
				t.Fatal("gate did not reach Kubernetes reconciliation")
			}
			// Kubernetes is still waiting; an unrelated durable mutation must commit.
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(4*time.Second))
			started := time.Now()
			_, writeErr := writeStore.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "test", Action: "writer.during.kubernetes", ResourceType: "test"})
			elapsed := time.Since(started)
			cancel()
			close(release)
			gateErr := <-gateDone
			if writeErr != nil {
				t.Fatalf("writer blocked by Kubernetes: %v", writeErr)
			}
			if gateErr != nil || continuity.reconcileCalls != reconcilesBefore+2 {
				t.Fatalf("head moved during Kubernetes: gate=%v, fresh proof checks=%d, want 2", gateErr, continuity.reconcileCalls-reconcilesBefore)
			}
			t.Logf("%s writer committed during blocked Kubernetes GET in %s; gate retried with fresh proof", writer, elapsed)
			if err := manager.AllowMutation(t.Context()); err != nil {
				t.Fatalf("fresh retry failed: %v", err)
			}
		})
	}
}
