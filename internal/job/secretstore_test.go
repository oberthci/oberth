package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/pkg/periapsis"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeSecretStoreFetcher struct {
	requested [][]string
	values    map[string]map[string][]byte
	err       error
}

func (fetcher *fakeSecretStoreFetcher) FetchKV(_ context.Context, paths []string) (map[string]map[string][]byte, error) {
	requested := make([]string, len(paths))
	copy(requested, paths)
	fetcher.requested = append(fetcher.requested, requested)
	if fetcher.err != nil {
		return nil, fetcher.err
	}
	result := make(map[string]map[string][]byte, len(paths))
	for _, path := range paths {
		values, ok := fetcher.values[path]
		if !ok {
			return nil, fmt.Errorf("secret store entry %q is unavailable: not found", path)
		}
		result[path] = values
	}
	return result, nil
}

func secretStoreConfig() Config {
	config := baseConfig()
	config.SecretStorePaths = []string{"ci/data/r2-upload", "ci/data/gar-push"}
	return config
}

func storeFetcherFixture() *fakeSecretStoreFetcher {
	return &fakeSecretStoreFetcher{values: map[string]map[string][]byte{
		"ci/data/r2-upload": {"token": []byte("r2-value")},
		"ci/data/gar-push":  {"json": []byte("gar-value")},
	}}
}

func declaredStoreFixture() []periapsis.SecretStoreDeclaration {
	return []periapsis.SecretStoreDeclaration{
		{Name: "gar-token", Path: "ci/data/gar-push"},
		{Name: "r2-token", Path: "ci/data/r2-upload"},
	}
}

func noopExec(context.Context, string, string, string, []string, io.Reader, io.Writer) error {
	return nil
}

func TestSecretStoreSnapshotFetchesAllowlistedDeclarations(t *testing.T) {
	t.Parallel()
	fetcher := storeFetcherFixture()
	controller, err := NewController(fake.NewSimpleClientset(), secretStoreConfig(), nil, fetcher, ExecStreamer(noopExec))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := controller.SecretStoreSnapshot(context.Background(), declaredStoreFixture(), []string{"gar-sa-key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Secrets) != 2 || snapshot.Secrets[0].Name != "gar-token" || snapshot.Secrets[1].Name != "r2-token" {
		t.Fatalf("snapshot = %+v", snapshot.Secrets)
	}
	if string(snapshot.Secrets[1].Values["token"]) != "r2-value" {
		t.Fatalf("fetched value = %q", snapshot.Secrets[1].Values["token"])
	}
	masks := snapshot.MaskValues()
	if len(masks) != 2 {
		t.Fatalf("mask values = %q", masks)
	}
	payload, err := snapshot.DeliveryPayload()
	if err != nil {
		t.Fatal(err)
	}
	var decoded runner.SecretStorePayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Secrets) != 2 || decoded.Secrets[0].Name != "gar-token" {
		t.Fatalf("payload = %+v", decoded.Secrets)
	}
}

func TestSecretStoreSnapshotFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		config   Config
		fetcher  SecretStoreFetcher
		declared []periapsis.SecretStoreDeclaration
		taken    []string
		want     string
	}{
		{
			name: "no fetcher configured", config: baseConfig(), fetcher: nil,
			declared: declaredStoreFixture(), want: "no secret store configured",
		},
		{
			name: "path outside allowlist", config: secretStoreConfig(), fetcher: storeFetcherFixture(),
			declared: []periapsis.SecretStoreDeclaration{{Name: "other", Path: "ci/data/other"}},
			want:     "not in the administrator allowlist",
		},
		{
			name: "missing store entry", config: secretStoreConfig(),
			fetcher:  &fakeSecretStoreFetcher{values: map[string]map[string][]byte{"ci/data/gar-push": {"json": []byte("x")}}},
			declared: declaredStoreFixture(), want: "is unavailable",
		},
		{
			name: "unreachable secret store", config: secretStoreConfig(),
			fetcher:  &fakeSecretStoreFetcher{err: errors.New("dial tcp: connection refused")},
			declared: declaredStoreFixture(), want: "release admission requires every declared secret store secret",
		},
		{
			name: "collision with kubernetes secret", config: secretStoreConfig(), fetcher: storeFetcherFixture(),
			declared: []periapsis.SecretStoreDeclaration{{Name: "gar-sa-key", Path: "ci/data/gar-push"}},
			taken:    []string{"gar-sa-key"}, want: "collides",
		},
		{
			name: "unsafe delivery name", config: secretStoreConfig(), fetcher: storeFetcherFixture(),
			declared: []periapsis.SecretStoreDeclaration{{Name: "Bad_Name!", Path: "ci/data/gar-push"}},
			want:     "DNS-1123",
		},
		{
			name: "empty fetched value", config: secretStoreConfig(),
			fetcher: &fakeSecretStoreFetcher{values: map[string]map[string][]byte{
				"ci/data/gar-push": {"json": {}}, "ci/data/r2-upload": {"token": []byte("x")},
			}},
			declared: declaredStoreFixture(), want: "empty value",
		},
		{
			name: "unsafe key", config: secretStoreConfig(),
			fetcher: &fakeSecretStoreFetcher{values: map[string]map[string][]byte{
				"ci/data/gar-push": {"..": []byte("x")}, "ci/data/r2-upload": {"token": []byte("x")},
			}},
			declared: declaredStoreFixture(), want: "unsafe",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			exec := ExecStreamer(noopExec)
			if test.fetcher == nil && len(test.config.SecretStorePaths) == 0 {
				exec = nil
			}
			controller, err := NewController(fake.NewSimpleClientset(), test.config, nil, test.fetcher, exec)
			if err != nil {
				t.Fatal(err)
			}
			_, err = controller.SecretStoreSnapshot(context.Background(), test.declared, test.taken)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSecretStoreSnapshotEmptyDeclarationNeedsNoFetcher(t *testing.T) {
	t.Parallel()
	controller, err := NewController(fake.NewSimpleClientset(), baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := controller.SecretStoreSnapshot(context.Background(), nil, nil)
	if err != nil || !snapshot.Empty() {
		t.Fatalf("empty declaration = %+v, %v", snapshot, err)
	}
}

func TestNewControllerRequiresDeliveryCapabilitiesForAllowlist(t *testing.T) {
	t.Parallel()
	config := secretStoreConfig()
	if _, err := NewController(fake.NewSimpleClientset(), config, nil, storeFetcherFixture(), nil); err == nil || !strings.Contains(err.Error(), "exec streamer") {
		t.Fatalf("missing exec streamer error = %v", err)
	}
	if _, err := NewController(fake.NewSimpleClientset(), config, nil, nil, ExecStreamer(noopExec)); err == nil || !strings.Contains(err.Error(), "secret store fetcher") {
		t.Fatalf("missing secret store fetcher error = %v", err)
	}
}

func secretStoreReleaseRequest() Request {
	request := baseRequest()
	request.Release = true
	request.Ref = "refs/tags/v1.2.3"
	request.ReleaseSecrets = testSecretSnapshot(request, []ReleaseSecret{{Name: "gar-sa-key", Keys: []string{"json"}}})
	request.SecretStoreSecrets = []SecretStoreSource{
		{Name: "gar-token", Path: "ci/data/gar-push"},
		{Name: "r2-token", Path: "ci/data/r2-upload"},
	}
	return request
}

func TestReleaseBuildMountsMemoryBackedVaultDeliveryVolume(t *testing.T) {
	t.Parallel()
	request := secretStoreReleaseRequest()
	built, err := Build(secretStoreConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	var storeVolume *corev1.Volume
	for index, volume := range built.Spec.Template.Spec.Volumes {
		if volume.Name == secretStoreVolumeName {
			storeVolume = &built.Spec.Template.Spec.Volumes[index]
		}
		if volume.Secret != nil && volume.Name == secretStoreVolumeName {
			t.Fatal("secretstore volume must never be a Kubernetes Secret")
		}
	}
	if storeVolume == nil || storeVolume.EmptyDir == nil || storeVolume.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("secretstore volume = %#v, want memory-backed emptyDir", storeVolume)
	}
	if storeVolume.EmptyDir.SizeLimit == nil || storeVolume.EmptyDir.SizeLimit.IsZero() {
		t.Fatalf("secretstore volume size limit = %v", storeVolume.EmptyDir.SizeLimit)
	}
	container := built.Spec.Template.Spec.Containers[0]
	foundMount := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == secretStoreVolumeName {
			foundMount = true
			if mount.MountPath != secretStoreMountPath || mount.ReadOnly {
				t.Fatalf("secretstore mount = %#v", mount)
			}
		}
	}
	if !foundMount {
		t.Fatal("vault delivery volume is not mounted")
	}
	arguments := strings.Join(container.Args, " ")
	if !strings.Contains(arguments, "--secretstore-dir "+secretStoreMountPath) {
		t.Fatalf("runner arguments = %q", arguments)
	}
	environment := make(map[string]string, len(container.Env))
	for _, variable := range container.Env {
		environment[variable.Name] = variable.Value
	}
	if environment["OBERTH_SECRETSTORE_DIR"] != secretStoreMountPath {
		t.Fatalf("environment = %#v", environment)
	}
	var annotated []SecretStoreSource
	if err := json.Unmarshal([]byte(built.Annotations[secretStoreAnnotation]), &annotated); err != nil {
		t.Fatalf("secretstore annotation = %q: %v", built.Annotations[secretStoreAnnotation], err)
	}
	if len(annotated) != 2 || annotated[0].Name != "gar-token" || annotated[0].Path != "ci/data/gar-push" {
		t.Fatalf("annotated sources = %+v", annotated)
	}
	if strings.Contains(built.Annotations[secretStoreAnnotation], "r2-value") {
		t.Fatal("annotation carries a secret value")
	}
}

func TestBuildRejectsUnsafeVaultSecretRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{name: "ci trigger", mutate: func(request *Request) {
			request.Release = false
			request.Ref = "feature/test"
			request.ReleaseSecrets = nil
		}, want: "CI request must not include secret store secret sources"},
		{name: "outside allowlist", mutate: func(request *Request) {
			request.SecretStoreSecrets = []SecretStoreSource{{Name: "other", Path: "ci/data/other"}}
		}, want: "not in the administrator allowlist"},
		{name: "unsorted names", mutate: func(request *Request) {
			request.SecretStoreSecrets = []SecretStoreSource{
				{Name: "r2-token", Path: "ci/data/r2-upload"},
				{Name: "gar-token", Path: "ci/data/gar-push"},
			}
		}, want: "strictly sorted"},
		{name: "collision with kubernetes mount", mutate: func(request *Request) {
			request.SecretStoreSecrets = []SecretStoreSource{{Name: "gar-sa-key", Path: "ci/data/gar-push"}}
		}, want: "collides"},
		{name: "duplicate path", mutate: func(request *Request) {
			request.SecretStoreSecrets = []SecretStoreSource{
				{Name: "a-token", Path: "ci/data/gar-push"},
				{Name: "b-token", Path: "ci/data/gar-push"},
			}
		}, want: "declared more than once"},
		{name: "traversal path", mutate: func(request *Request) {
			request.SecretStoreSecrets = []SecretStoreSource{{Name: "gar-token", Path: "ci/../data"}}
		}, want: "clean non-empty segments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := secretStoreReleaseRequest()
			test.mutate(&request)
			if _, err := Build(secretStoreConfig(), request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWaitDeliversSecretStorePayloadBeforeCompletion(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "release-demo-run", Namespace: "oberth", ResourceVersion: "1",
		Annotations: map[string]string{jobRunIDAnnotation: "run-1"},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "release-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": running.Name,
		}, Annotations: map[string]string{jobRunIDAnnotation: "run-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(running, pod)
	jobWatcher := watch.NewRaceFreeFake()
	watchReady := make(chan struct{})
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		select {
		case <-watchReady:
		default:
			close(watchReady)
		}
		return true, jobWatcher, nil
	})

	var mu sync.Mutex
	var deliveredPayload []byte
	var deliveredCommand []string
	delivered := make(chan struct{})
	exec := func(_ context.Context, namespace, podName, container string, command []string, stdin io.Reader, _ io.Writer) error {
		payload, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		mu.Lock()
		deliveredPayload = payload
		deliveredCommand = command
		mu.Unlock()
		if namespace != "oberth" || podName != pod.Name || container != "run" {
			return fmt.Errorf("exec target = %s/%s/%s", namespace, podName, container)
		}
		select {
		case <-delivered:
		default:
			close(delivered)
		}
		return nil
	}
	go func() {
		<-watchReady
		<-delivered
		finishedPod := pod.DeepCopy()
		finishedPod.Status.Phase = corev1.PodSucceeded
		finishedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed", Message: successfulTerminationMessage,
			}},
		}}
		_, _ = client.CoreV1().Pods("oberth").Update(context.Background(), finishedPod, metav1.UpdateOptions{})
		terminal := running.DeepCopy()
		terminal.ResourceVersion = "2"
		terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		jobWatcher.Modify(terminal)
	}()

	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	config := secretStoreConfig()
	config.JobTimeout = 5 * time.Second
	controller, err := NewController(client, config, streamer, storeFetcherFixture(), exec)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"version":1,"secrets":[{"name":"r2-token","keys":{"token":"djE="}}]}`)
	completion, err := controller.Wait(context.Background(), running.Name, "run-1", io.Discard, nil, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !completion.Succeeded {
		t.Fatalf("completion = %+v", completion)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(deliveredPayload) != string(payload) {
		t.Fatalf("delivered payload = %q", deliveredPayload)
	}
	wantCommand := []string{runnerBinaryPath, "--receive-secretstore", secretStoreMountPath}
	if len(deliveredCommand) != 3 || deliveredCommand[0] != wantCommand[0] || deliveredCommand[1] != wantCommand[1] || deliveredCommand[2] != wantCommand[2] {
		t.Fatalf("delivery command = %q", deliveredCommand)
	}
}

func TestWaitSurfacesTerminalDeliveryFailureOnFailedJob(t *testing.T) {
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "release-demo-run", Namespace: "oberth", ResourceVersion: "1",
			Annotations: map[string]string{jobRunIDAnnotation: "run-1"},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "release-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": failed.Name,
		}, Annotations: map[string]string{jobRunIDAnnotation: "run-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}},
	}
	client := fake.NewSimpleClientset(failed, pod)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	exec := func(context.Context, string, string, string, []string, io.Reader, io.Writer) error {
		return errors.New("exec transport refused")
	}
	config := secretStoreConfig()
	config.JobTimeout = 5 * time.Second
	controller, err := NewController(client, config, streamer, storeFetcherFixture(), exec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.Wait(context.Background(), failed.Name, "run-1", io.Discard, nil, []byte(`{"version":1}`))
	if err == nil || !strings.Contains(err.Error(), "secret store delivery") {
		t.Fatalf("error = %v, want secret store delivery diagnostics", err)
	}
}
