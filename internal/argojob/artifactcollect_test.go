package argojob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCollectStreamsTheArtifactsDirectoryOut(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	var command []string
	exec := func(_ context.Context, _, _, _ string, argv []string,
		_ io.Reader, stdout, _ io.Writer,
	) error {
		command = argv
		_, err := stdout.Write([]byte("archive-bytes"))
		return err
	}
	seeder := NewSourceSeeder(client, exec, seedTestConfig())

	body, err := seeder.Collect(context.Background(), "oberth-run-abc")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if string(body) != "archive-bytes" {
		t.Fatalf("Collect returned %q", body)
	}
	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "tar -czf -") {
		t.Fatalf("Collect does not create an archive: %v", command)
	}
	if !strings.Contains(joined, artifactsSubPath) {
		t.Fatalf("Collect does not read the artifacts directory: %v", command)
	}
}

func TestCollectPodHoldsNoServiceAccountToken(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string,
		_ io.Reader, stdout, _ io.Writer,
	) error {
		_, err := stdout.Write([]byte("x"))
		return err
	}, seedTestConfig())

	if _, err := seeder.Collect(context.Background(), "oberth-run-abc"); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	created := createdPods(t, client)
	if len(created) == 0 {
		t.Fatal("no collecting Pod was created")
	}
	for _, pod := range created {
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Fatalf("collecting Pod %s carries a ServiceAccount token", pod.Name)
		}
	}
}

func TestCollectDeletesItsPodEvenWhenTheExecFails(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string,
		_ io.Reader, _, _ io.Writer,
	) error {
		return errors.New("exec exploded")
	}, seedTestConfig())

	if _, err := seeder.Collect(context.Background(), "oberth-run-abc"); err == nil {
		t.Fatal("Collect reported success after the exec failed")
	}
	pods, err := client.CoreV1().Pods(seedTestConfig().Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if strings.Contains(pod.Name, "collect") {
			t.Fatalf("collecting Pod %s was left behind", pod.Name)
		}
	}
}

func TestCollectReturnsNothingForAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string,
		_ io.Reader, _, _ io.Writer,
	) error {
		return nil
	}, seedTestConfig())

	body, err := seeder.Collect(context.Background(), "oberth-run-abc")
	if err != nil {
		t.Fatalf("an empty artifacts directory should not be an error: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("Collect returned %d bytes for an empty directory", len(body))
	}
}

func TestCollectRefusesAnOversizeStream(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	config := seedTestConfig()
	config.ArtifactsLimitBytes = 16
	seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string,
		_ io.Reader, stdout, _ io.Writer,
	) error {
		_, err := stdout.Write(bytes.Repeat([]byte("x"), 1024))
		return err
	}, config)

	if _, err := seeder.Collect(context.Background(), "oberth-run-abc"); err == nil {
		t.Fatal("Collect accepted a stream past the configured limit")
	}
}

func TestCollectMountsTheRunClaim(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string,
		_ io.Reader, stdout, _ io.Writer,
	) error {
		_, err := stdout.Write([]byte("x"))
		return err
	}, seedTestConfig())

	if _, err := seeder.Collect(context.Background(), "oberth-run-abc"); err != nil {
		t.Fatal(err)
	}
	want := sourceClaimName("oberth-run-abc")
	found := false
	for _, pod := range createdPods(t, client) {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == want {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no collecting Pod mounted the run claim %s", want)
	}
}

func createdPods(t *testing.T, client *fake.Clientset) []*corev1.Pod {
	t.Helper()
	var pods []*corev1.Pod
	for _, action := range client.Actions() {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetResource().Resource != "pods" {
			continue
		}
		if pod, ok := create.GetObject().(*corev1.Pod); ok {
			pods = append(pods, pod)
		}
	}
	return pods
}
