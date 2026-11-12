package argojob

import (
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSourceSeedPendingBeyondOldStartupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := seedTestConfig()
		config.SeedPollInterval = time.Minute
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: config.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
		client := fake.NewClientset(pod)
		seeder := &SourceSeeder{client: client, config: config}
		result := make(chan error, 1)
		go func() { result <- seeder.waitForSeedPod(t.Context(), pod.Name) }()
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("ordinary Pending Pod expired: %v", err)
		default:
		}
		pod.Status.Phase = corev1.PodRunning
		if _, err := client.CoreV1().Pods(config.Namespace).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}
