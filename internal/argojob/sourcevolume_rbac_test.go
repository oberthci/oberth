package argojob

import (
	"context"
	"errors"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// The frozen PVC update verb is live authority, not an unused permission:
// removing it prevents Workflow ownership and hence garbage collection.
func TestSourceClaimAdoptionRequiresPVCUpdate(t *testing.T) {
	for _, allowUpdate := range []bool{false, true} {
		t.Run(map[bool]string{false: "forbidden", true: "allowed"}[allowUpdate], func(t *testing.T) {
			config := seedTestConfig()
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "run-src", Namespace: config.Namespace}}
			client := fake.NewClientset(claim)
			updates := 0
			client.PrependReactor("update", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
				updates++
				if !allowUpdate {
					return true, nil, errors.New("PVC update forbidden")
				}
				return false, nil, nil
			})
			seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return nil
			}, config)
			err := seeder.AdoptClaim(t.Context(), claim.Name, "run", "workflow-uid")
			if (err == nil) != allowUpdate || updates != 1 {
				t.Fatalf("allowed=%t updates=%d err=%v", allowUpdate, updates, err)
			}
			after, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).Get(t.Context(), claim.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if allowUpdate {
				if len(after.OwnerReferences) != 1 || after.OwnerReferences[0].UID != "workflow-uid" || after.OwnerReferences[0].Kind != "Workflow" {
					t.Fatalf("missing Workflow ownership: %+v", after.OwnerReferences)
				}
			} else if len(after.OwnerReferences) != 0 {
				t.Fatal("forbidden update unexpectedly adopted claim")
			}
		})
	}
}
