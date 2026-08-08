package job

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/redact"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

const successfulTerminationMessage = `[{"burn":"test","step":"go-test","status":"passed","exit_code":0,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"}]`

func TestControllerCreateAndCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	name, err := controller.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.BatchV1().Jobs("oberth").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil || created.Spec.BackoffLimit == nil || *created.Spec.BackoffLimit != 0 {
		t.Fatalf("created Job = %#v, %v", created, err)
	}
	if err := controller.Cancel(context.Background(), name, request.RunID); err != nil {
		t.Fatal(err)
	}
	if err := controller.Cancel(context.Background(), name, request.RunID); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
}

func TestControllerCancelWaitsForOwnedPodToStop(t *testing.T) {
	jobUID := types.UID("job-cancel-uid")
	jobName := "oberth-cancel-running"
	runID := "run-cancel-running"
	controllerRef := true
	runningJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: jobName, Namespace: "oberth", UID: jobUID, Annotations: map[string]string{jobRunIDAnnotation: runID},
	}}
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-cancel-running-pod", Namespace: "oberth",
			Labels:      map[string]string{"job-name": jobName},
			Annotations: map[string]string{jobRunIDAnnotation: runID},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: jobUID, Controller: &controllerRef,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(runningJob, runningPod)
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Cancel(ctx, jobName, runID) }()

	select {
	case err := <-done:
		t.Fatalf("Cancel returned while its owned Pod was still running: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	stoppedPod := runningPod.DeepCopy()
	stoppedPod.Status.Phase = corev1.PodSucceeded
	stoppedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  runnerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if _, err := client.CoreV1().Pods("oberth").Update(ctx, stoppedPod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("Cancel did not observe the stopped Pod: %v", ctx.Err())
	}

	var sawBoundForegroundDelete bool
	for _, action := range client.Actions() {
		deletion, ok := action.(k8stesting.DeleteAction)
		if !ok || action.GetResource().Resource != "jobs" {
			continue
		}
		options := deletion.GetDeleteOptions()
		if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground ||
			options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != jobUID {
			t.Fatalf("Job delete options = %#v, want foreground propagation bound to UID %q", options, jobUID)
		}
		sawBoundForegroundDelete = true
	}
	if !sawBoundForegroundDelete {
		t.Fatal("Cancel did not issue a bound foreground Job delete")
	}
}

func TestControllerCancelRejectsSameNameFromDifferentRun(t *testing.T) {
	for _, annotatedRunID := range []string{"replacement-run", ""} {
		name := "nonempty-mismatch"
		if annotatedRunID == "" {
			name = "present-empty"
		}
		t.Run(name, func(t *testing.T) {
			jobName := "oberth-reused-name"
			expectedRunID := "expected-run"
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: jobName, Namespace: "oberth", UID: types.UID("replacement-uid"),
				Labels: map[string]string{"oberth.ci/run": labelValue(expectedRunID)},
				Annotations: map[string]string{
					jobRunIDAnnotation: annotatedRunID, jobSpecIdentityAnnotation: strings.Repeat("a", 64),
				},
			}}
			client := fake.NewSimpleClientset(job)
			controller, err := NewController(client, baseConfig(), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = controller.Cancel(context.Background(), jobName, expectedRunID)
			if err == nil || !strings.Contains(err.Error(), "different durable run") {
				t.Fatalf("same-name replacement cancellation error = %v", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "delete" {
					t.Fatalf("different-run Job was deleted: %#v", action)
				}
			}
		})
	}
}

func TestControllerCancelAcceptsBoundJobFromRollingUpgrade(t *testing.T) {
	jobName := "oberth-upgrade-owner"
	runID := "run-before-upgrade"
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: jobName, Namespace: "oberth", UID: types.UID("upgrade-owner-uid"),
		Labels:      map[string]string{"oberth.ci/run": labelValue(runID)},
		Annotations: map[string]string{jobSpecIdentityAnnotation: strings.Repeat("a", 64)},
	}}
	client := fake.NewSimpleClientset(job)
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Cancel(context.Background(), jobName, runID); err != nil {
		t.Fatalf("cancel rolling-upgrade Job: %v", err)
	}
	var deleted bool
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "jobs" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("rolling-upgrade Job was not deleted")
	}
}

func TestControllerCancelWaitsForLegacyOrphanPod(t *testing.T) {
	jobName := "oberth-upgrade-orphan"
	runID := "run-upgrade-orphan"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-upgrade-orphan-pod", Namespace: "oberth",
			Labels: map[string]string{"job-name": jobName, "oberth.ci/run": labelValue(runID)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(pod)
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Cancel(ctx, jobName, runID) }()
	select {
	case err := <-done:
		t.Fatalf("Cancel returned while legacy orphan Pod was running: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	stopped := pod.DeepCopy()
	stopped.Status.Phase = corev1.PodFailed
	if _, err := client.CoreV1().Pods("oberth").Update(ctx, stopped, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("Cancel did not observe legacy orphan Pod termination: %v", ctx.Err())
	}
}

func TestControllerReconcilesJobWhenCreateResponseIsLost(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		created.UID = types.UID("persisted-before-timeout")
		if err := client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), created, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("create response lost", 1)
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	request.JobName = "oberth-response-lost"
	name, err := controller.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if name != request.JobName {
		t.Fatalf("reconciled Job name = %q, want %q", name, request.JobName)
	}
}

func TestControllerProvisionsOnlyTheRepositoryCache(t *testing.T) {
	root := t.TempDir()
	config := baseConfig()
	config.CICacheRoot = filepath.Join(root, "ci")
	config.ReleaseCacheRoot = filepath.Join(root, "release")
	config.ProvisionCacheDirs = true
	for _, cacheRoot := range []string{config.CICacheRoot, config.ReleaseCacheRoot} {
		if err := os.Mkdir(cacheRoot, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	controller, err := NewController(fake.NewSimpleClientset(), config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	if _, err := controller.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Join(config.CICacheRoot, "repos", repositoryCacheKey(request.Repo), "feature")
	for _, cache := range []string{"gomod", "gobuild"} {
		info, err := os.Stat(filepath.Join(repositoryRoot, cache))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
			t.Fatalf("provisioned %s cache = %#v, %v", cache, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config.ReleaseCacheRoot, "repos", repositoryCacheKey(request.Repo))); !os.IsNotExist(err) {
		t.Fatalf("CI admission provisioned release cache: %v", err)
	}
}

func TestControllerRejectsSymlinkedRepositoryCacheParent(t *testing.T) {
	root := t.TempDir()
	config := baseConfig()
	config.CICacheRoot = filepath.Join(root, "ci")
	config.ReleaseCacheRoot = filepath.Join(root, "release")
	config.ProvisionCacheDirs = true
	outside := filepath.Join(root, "outside")
	for _, directory := range []string{config.CICacheRoot, config.ReleaseCacheRoot, outside} {
		if err := os.Mkdir(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(config.CICacheRoot, "repos")); err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(fake.NewSimpleClientset(), config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Create(context.Background(), baseRequest()); err == nil {
		t.Fatal("Create followed a symlinked cache parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside cache entries = %#v, %v", entries, err)
	}
}

func TestControllerCreateIsIdempotentOnlyForMatchingSpec(t *testing.T) {
	client := fake.NewSimpleClientset()
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	request.JobName = "oberth-" + request.RunID

	first, err := controller.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("retry identical create: %v", err)
	}
	if first != request.JobName || second != first {
		t.Fatalf("created names = %q and %q, want %q", first, second, request.JobName)
	}

	request.SHA = strings.Repeat("b", 40)
	if _, err := controller.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "different spec identity") {
		t.Fatalf("mismatched retry error = %v", err)
	}
}

func TestControllerCreateRejectsUnownedNameCollision(t *testing.T) {
	request := baseRequest()
	request.JobName = "oberth-" + request.RunID
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: request.JobName, Namespace: "oberth"}})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "different spec identity") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestSecretSnapshotFailClosedAndCollectsSortedKeysAndEveryDataValue(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"}, Data: map[string][]byte{"user": []byte("xy"), "json": []byte("secret-one")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r2-upload-token", Namespace: "oberth"}, Data: map[string][]byte{"token": []byte("secret-two")}},
	)
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key", "r2-upload-token"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const jobName = "oberth-release-run"
	snapshot, err := controller.SecretSnapshot(context.Background(), jobName, []string{"r2-upload-token", "gar-sa-key"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != releaseSnapshotName(jobName) || len(snapshot.Digest) != 64 {
		t.Fatalf("snapshot identity = %q/%q", snapshot.Name, snapshot.Digest)
	}
	wantSecrets := []ReleaseSecret{
		{Name: "gar-sa-key", Keys: []string{"json", "user"}},
		{Name: "r2-upload-token", Keys: []string{"token"}},
	}
	if !reflect.DeepEqual(snapshot.Mounts.Secrets, wantSecrets) {
		t.Fatalf("secret keys = %#v, want %#v", snapshot.Mounts.Secrets, wantSecrets)
	}
	gotValues := make(map[string]bool, len(snapshot.Data))
	for _, value := range snapshot.MaskValues() {
		gotValues[string(value)] = true
	}
	for _, want := range []string{"xy", "secret-one", "secret-two"} {
		if !gotValues[want] {
			t.Fatalf("mask values = %q, missing %q", snapshot.MaskValues(), want)
		}
	}
	config.ReleaseSecrets = append(config.ReleaseSecrets, "missing")
	controller, _ = NewController(client, config, nil, nil, nil)
	if _, err := controller.SecretSnapshot(context.Background(), jobName, config.ReleaseSecrets); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretSnapshotMountsOnlyRepositoryDeclaredAllowedSubset(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"}, Data: map[string][]byte{"json": []byte("must-not-leak")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r2-upload-token", Namespace: "oberth"}, Data: map[string][]byte{"token": []byte("declared")}},
	)
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key", "r2-upload-token"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := controller.SecretSnapshot(context.Background(), "oberth-release-cli", []string{"r2-upload-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Mounts.Secrets, []ReleaseSecret{{Name: "r2-upload-token", Keys: []string{"token"}}}) {
		t.Fatalf("subset snapshot mounts = %#v", snapshot.Mounts.Secrets)
	}
	for _, value := range snapshot.MaskValues() {
		if string(value) == "must-not-leak" {
			t.Fatal("undeclared GAR credential entered the release snapshot")
		}
	}
	if _, err := controller.SecretSnapshot(context.Background(), "oberth-release-cli", []string{"not-allowed"}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("non-allowed repository request error = %v", err)
	}
}

func TestControllerCreatesImmutableOwnedReleaseSnapshotAndMountsOnlyIt(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"}, Data: map[string][]byte{"json": []byte("line-one\nline-two")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r2-upload-token", Namespace: "oberth"}, Data: map[string][]byte{"token": []byte("token-value")}},
	)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).UID = types.UID("job-uid-1")
		return false, nil, nil
	})
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key", "r2-upload-token"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	request.JobName = "oberth-release-run"
	request.Ref = "refs/tags/v1.2.3"
	request.Release = true
	snapshot, err := controller.SecretSnapshot(context.Background(), request.JobName, config.ReleaseSecrets)
	if err != nil {
		t.Fatal(err)
	}
	request.ReleaseSecrets = &snapshot
	if _, err := controller.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	createdSecret, err := client.CoreV1().Secrets("oberth").Get(context.Background(), snapshot.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if createdSecret.Immutable == nil || !*createdSecret.Immutable || !reflect.DeepEqual(createdSecret.Data, snapshot.Data) {
		t.Fatalf("immutable snapshot = %#v", createdSecret)
	}
	if len(createdSecret.OwnerReferences) != 1 || createdSecret.OwnerReferences[0].Name != request.JobName || createdSecret.OwnerReferences[0].UID != types.UID("job-uid-1") || createdSecret.OwnerReferences[0].Controller == nil || !*createdSecret.OwnerReferences[0].Controller {
		t.Fatalf("snapshot owner = %#v", createdSecret.OwnerReferences)
	}
	createdJob, err := client.BatchV1().Jobs("oberth").Get(context.Background(), request.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var mountedSecrets []string
	for _, volume := range createdJob.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			mountedSecrets = append(mountedSecrets, volume.Secret.SecretName)
		}
	}
	if !reflect.DeepEqual(mountedSecrets, []string{snapshot.Name}) {
		t.Fatalf("mounted Secrets = %q, want only snapshot %q", mountedSecrets, snapshot.Name)
	}

	source, _ := client.CoreV1().Secrets("oberth").Get(context.Background(), "gar-sa-key", metav1.GetOptions{})
	source.Data["json"] = []byte("rotated")
	if _, err := client.CoreV1().Secrets("oberth").Update(context.Background(), source, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	stillSnapshot, _ := client.CoreV1().Secrets("oberth").Get(context.Background(), snapshot.Name, metav1.GetOptions{})
	if !reflect.DeepEqual(stillSnapshot.Data, snapshot.Data) {
		t.Fatalf("snapshot changed after source rotation: %q", stillSnapshot.Data)
	}

	if err := controller.Cancel(context.Background(), request.JobName, request.RunID); err != nil {
		t.Fatal(err)
	}
	remainingSnapshot, err := client.CoreV1().Secrets("oberth").Get(context.Background(), snapshot.Name, metav1.GetOptions{})
	if err != nil || len(remainingSnapshot.OwnerReferences) != 1 || remainingSnapshot.OwnerReferences[0].Name != request.JobName {
		t.Fatalf("snapshot must remain owner-bound for Kubernetes GC: %#v, %v", remainingSnapshot, err)
	}
	if err := controller.Cancel(context.Background(), request.JobName, request.RunID); err != nil {
		t.Fatalf("idempotent cancellation: %v", err)
	}
}

func TestControllerReconcilesMatchingReleaseSnapshotWhenCreateResponseIsLost(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"}, Data: map[string][]byte{"json": []byte("release-value")}},
	)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).UID = types.UID("release-job-uid")
		return false, nil, nil
	})
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy()
		if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("secrets"), created, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("create response lost", 1)
	})
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	request.JobName, request.Ref, request.Release = "oberth-release-response-lost", "refs/tags/v1.2.3", true
	snapshot, err := controller.SecretSnapshot(context.Background(), request.JobName, config.ReleaseSecrets)
	if err != nil {
		t.Fatal(err)
	}
	request.ReleaseSecrets = &snapshot
	if _, err := controller.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestControllerReleaseSnapshotCollisionFailsClosedWithoutDeletingUnownedSecret(t *testing.T) {
	request := baseRequest()
	request.JobName = "oberth-release-collision"
	request.Ref = "refs/tags/v1.2.3"
	request.Release = true
	immutable := true
	client := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"}, Data: map[string][]byte{"json": []byte("admission-value")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: releaseSnapshotName(request.JobName), Namespace: "oberth"}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"wrong": []byte("unowned")}},
	)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).UID = types.UID("job-uid-collision")
		return false, nil, nil
	})
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := controller.SecretSnapshot(context.Background(), request.JobName, config.ReleaseSecrets)
	if err != nil {
		t.Fatal(err)
	}
	request.ReleaseSecrets = &snapshot
	if _, err := controller.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "different immutable content or ownership") {
		t.Fatalf("collision error = %v", err)
	}
	if _, err := client.BatchV1().Jobs("oberth").Get(context.Background(), request.JobName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("release Job survived failed snapshot creation: %v", err)
	}
	if _, err := client.CoreV1().Secrets("oberth").Get(context.Background(), snapshot.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("unowned colliding Secret was deleted: %v", err)
	}
}

func TestSecretSnapshotRejectsUnsafeDataKey(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gar-sa-key", Namespace: "oberth"},
		Data:       map[string][]byte{"../token": []byte("secret")},
	})
	config := baseConfig()
	config.ReleaseSecrets = []string{"gar-sa-key"}
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SecretSnapshot(context.Background(), "oberth-release-run", config.ReleaseSecrets); err == nil || !strings.Contains(err.Error(), "data key") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewControllerRejectsInvalidOrDuplicateReleaseSecretNames(t *testing.T) {
	for _, secrets := range [][]string{{"Gar-Key"}, {"gar_key"}, {"gar-key", "gar-key"}} {
		config := baseConfig()
		config.ReleaseSecrets = secrets
		if _, err := NewController(fake.NewSimpleClientset(), config, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "release Secret") {
			t.Fatalf("secrets = %q, error = %v", secrets, err)
		}
	}
}

func TestCopyLogsRedactsEveryKnownValueAcrossReadBoundaries(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
	}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(&chunkReader{chunks: [][]byte{
			[]byte("before sec"), []byte("ret-value and x"), []byte("y after"),
		}}), nil
	}
	controller, err := NewController(fake.NewSimpleClientset(pod), baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	podName, err := controller.copyLogs(context.Background(), "run-1", &output, [][]byte{[]byte("secret-value"), []byte("xy")}, &logCopyState{})
	if err != nil {
		t.Fatal(err)
	}
	if podName != pod.Name || output.String() != "before *** and *** after" {
		t.Fatalf("pod/output = %q/%q", podName, output.String())
	}
}

func TestCopyLogsPublishesCurrentBurnAndStepFromChunkedLog(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-acme-cli-2d2f0986-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	body := "plain output before a marker\n[lint/vet] $ go vet ./...\n[lint/vet] passed\n[test/unit_race] $ go test -race ./...\n"
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(&chunkReader{chunks: [][]byte{
			[]byte("plain output before a mark"), []byte("er\n[li"), []byte("nt/vet]"), []byte(" $"),
			[]byte(" go vet ./...\n[lint/vet] passed\n[te"), []byte("st/unit_race] "), []byte("$ go test -race ./...\n"),
		}}), nil
	}
	client := fake.NewSimpleClientset(pod)
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := controller.copyLogs(context.Background(), "run-1", &output, nil, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	current := waitForPodProgress(t, client, pod.Name, podProgress{burn: "test", step: "unit_race"})
	if output.String() != body || current.Labels["oberth.ci/burn"] != "test" || current.Labels["oberth.ci/step"] != "unit_race" ||
		current.Annotations["oberth.ci/current-burn"] != "test" || current.Annotations["oberth.ci/current-step"] != "unit_race" {
		t.Fatalf("output/metadata = %q/%#v/%#v", output.String(), current.Labels, current.Annotations)
	}
}

func TestCopyLogsKeepsFailedStepWhenLaterStepsAreSkipped(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-acme-cli-2d2f0986-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}},
	}
	body := "[test/unit] $ false\n[test/unit] failed with exit code 1\n[test/integration] skipped after earlier failure\n[release/publish] skipped after earlier failure\n"
	client := fake.NewSimpleClientset(pod)
	controller, err := NewController(client, baseConfig(), func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.copyLogs(context.Background(), "run-1", io.Discard, nil, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	current := waitForPodProgress(t, client, pod.Name, podProgress{burn: "test", step: "unit"})
	if current.Annotations["oberth.ci/current-burn"] != "test" || current.Annotations["oberth.ci/current-step"] != "unit" {
		t.Fatalf("skipped steps replaced failed-step metadata: %#v", current.Annotations)
	}
}

func TestCopyLogsTreatsPodProgressPatchFailureAsBestEffort(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oberth-acme-cli-2d2f0986-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("metadata API unavailable")
	})
	body := "[test/unit] $ go test ./...\n[test/unit] retained output\n"
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := controller.copyLogs(context.Background(), "run-1", &output, nil, &logCopyState{}); err != nil {
		t.Fatalf("cosmetic Pod metadata failed the log stream: %v", err)
	}
	if output.String() != body {
		t.Fatalf("retained output = %q", output.String())
	}
}

func TestLogProgressWriterSeesOnlyRedactedBytes(t *testing.T) {
	const secret = "release-secret"
	reporter := &podProgressReporter{updates: make(chan podProgress, 1)}
	progressWriter := &logProgressWriter{reporter: reporter}
	redactor := redact.NewWriter(progressWriter, [][]byte{[]byte(secret)})
	if _, err := redactor.Write([]byte("[release-")); err != nil {
		t.Fatal(err)
	}
	if _, err := redactor.Write([]byte("secret/unit] $ publish\n")); err != nil {
		t.Fatal(err)
	}
	if err := redactor.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case progress := <-reporter.updates:
		if progress.burn != "***" || progress.step != "unit" || strings.Contains(progress.burn+progress.step, secret) {
			t.Fatalf("published progress = %#v, want redacted burn and unit step", progress)
		}
	default:
		t.Fatal("redacted runner start did not publish progress")
	}
}

func TestPodProgressReporterCoalescesWhilePatchIsBlocked(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "run-pod", Namespace: "oberth"}}
	client := fake.NewSimpleClientset(pod)
	firstPatchStarted := make(chan struct{})
	releaseFirstPatch := make(chan struct{})
	patches := 0
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 1 {
			close(firstPatchStarted)
			<-releaseFirstPatch
		}
		return false, nil, nil
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := newPodProgressReporter(context.Background(), controller, pod.Name)
	reporter.publish(podProgress{burn: "test", step: "first"})
	select {
	case <-firstPatchStarted:
	case <-time.After(time.Second):
		t.Fatal("first Pod progress patch did not start")
	}
	reporter.publish(podProgress{burn: "test", step: "second"})
	reporter.publish(podProgress{burn: "test", step: "latest"})
	closed := make(chan struct{})
	go func() {
		reporter.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		close(releaseFirstPatch)
		t.Fatal("closing the cosmetic progress reporter waited for a blocked patch")
	}
	close(releaseFirstPatch)
	select {
	case <-reporter.done:
	case <-time.After(3 * time.Second):
		t.Fatal("progress reporter did not stop after bounded patch requests")
	}
	if patches != 2 {
		t.Fatalf("Pod progress patches = %d, want first and coalesced latest", patches)
	}
	current, err := client.CoreV1().Pods("oberth").Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.Labels["oberth.ci/burn"] != "test" || current.Labels["oberth.ci/step"] != "latest" {
		t.Fatalf("coalesced Pod progress = %#v, want latest", current.Labels)
	}
}

func TestWaitIgnoresBlockedPodProgressPatch(t *testing.T) {
	terminal := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": terminal.Name,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed", Message: successfulTerminationMessage,
			}},
		}}},
	}
	patchStarted := make(chan struct{})
	releasePatch := make(chan struct{})
	patchReturned := make(chan struct{})
	client := &blockingPodPatchClient{
		Interface: fake.NewSimpleClientset(terminal, pod), pod: pod,
		started: patchStarted, release: releasePatch, returned: patchReturned,
	}
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(&eofAfterSignalReader{
			body: []byte("[test/unit] $ go test ./...\n"), signal: patchStarted,
		}), nil
	}
	config := baseConfig()
	config.JobTimeout = 500 * time.Millisecond
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type waitResult struct {
		completion Completion
		err        error
	}
	done := make(chan waitResult, 1)
	go func() {
		completion, waitErr := controller.Wait(context.Background(), terminal.Name, "run-1", io.Discard, nil, nil)
		done <- waitResult{completion: completion, err: waitErr}
	}()
	select {
	case <-patchStarted:
	case <-time.After(time.Second):
		close(releasePatch)
		t.Fatal("Pod progress patch did not block as required by the regression")
	}
	var result waitResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		close(releasePatch)
		result = <-done
		t.Fatalf("blocked cosmetic Pod patch delayed Wait: %v", result.err)
	}
	close(releasePatch)
	select {
	case <-patchReturned:
	case <-time.After(time.Second):
		t.Fatal("blocked Pod patch did not finish during test cleanup")
	}
	if result.err != nil || !result.completion.Succeeded || result.completion.ExitCode != 0 {
		t.Fatalf("completion/error = %#v/%v", result.completion, result.err)
	}
}

func TestCopyLogsRetriesSuccessfulEmptyStreamWhileRunnerIsWaiting(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		if openCalls == 2 {
			finished := pod.DeepCopy()
			finished.Status.Phase = corev1.PodSucceeded
			finished.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}
			if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), finished, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return io.NopCloser(&chunkReader{chunks: [][]byte{[]byte("complete sec"), []byte("ret-value log")}}), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output strings.Builder
	podName, err := controller.copyLogs(ctx, "run-1", &output, [][]byte{[]byte("secret-value")}, &logCopyState{})
	if err != nil {
		t.Fatal(err)
	}
	if podName != pod.Name || openCalls != 3 || output.String() != "complete *** log" {
		t.Fatalf("pod/calls/output = %q/%d/%q", podName, openCalls, output.String())
	}
}

func TestCopyLogsRetriesWhenRunnerStartsAfterPreStartEmptyStream(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			started := pod.DeepCopy()
			started.Status.Phase = corev1.PodRunning
			started.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
			if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), started, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
			return io.NopCloser(strings.NewReader("")), nil
		}
		if openCalls == 2 {
			finished := pod.DeepCopy()
			finished.Status.Phase = corev1.PodSucceeded
			finished.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}
			if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), finished, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return io.NopCloser(strings.NewReader("complete log")), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output strings.Builder
	if _, err := controller.copyLogs(ctx, "run-1", &output, nil, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	if openCalls != 3 || output.String() != "complete log" {
		t.Fatalf("calls/output = %d/%q", openCalls, output.String())
	}
}

func TestCopyLogsReplaysPrefixAndRedactsSecretAcrossReconnect(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return io.NopCloser(&readThenError{
				body: []byte("before sec"), err: errors.New("log stream reset"), failed: make(chan struct{}),
			}), nil
		}
		if openCalls == 2 {
			finished := pod.DeepCopy()
			finished.Status.Phase = corev1.PodSucceeded
			finished.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}
			if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), finished, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return io.NopCloser(strings.NewReader("before secret-value after")), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := controller.copyLogs(context.Background(), "run-1", &output, [][]byte{[]byte("secret-value")}, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	if openCalls != 3 || output.String() != "before *** after" {
		t.Fatalf("calls/output = %d/%q", openCalls, output.String())
	}
}

func TestCopyLogsRejectsChangedReplayPrefix(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return io.NopCloser(&readThenError{
				body: []byte("stable prefix"), err: errors.New("log stream reset"), failed: make(chan struct{}),
			}), nil
		}
		return io.NopCloser(strings.NewReader("changed prefix")), nil
	}
	controller, err := NewController(fake.NewSimpleClientset(pod), baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.copyLogs(context.Background(), "run-1", io.Discard, nil, &logCopyState{})
	if !errors.Is(err, errPodLogReplayChanged) || openCalls != 2 {
		t.Fatalf("error/calls = %v/%d", err, openCalls)
	}
}

func TestCopyLogsCancellationStopsEmptyStreamRetry(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	firstClosed := make(chan struct{})
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return &closeSignalReader{closed: firstClosed}, nil
		}
		return io.NopCloser(strings.NewReader("unexpected retry")), nil
	}
	controller, err := NewController(fake.NewSimpleClientset(pod), baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, copyErr := controller.copyLogs(ctx, "run-1", io.Discard, nil, &logCopyState{})
		done <- copyErr
	}()
	<-firstClosed
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if openCalls != 1 {
		t.Fatalf("open calls = %d, want 1", openCalls)
	}
}

func TestCopyLogsReturnsInspectionFailureAfterEmptyStream(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	inspectFailure := errors.New("pod inspection unavailable")
	client := fake.NewSimpleClientset(pod)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, inspectFailure
	})
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		return io.NopCloser(strings.NewReader("")), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = controller.copyLogs(ctx, "run-1", io.Discard, nil, &logCopyState{})
	if !errors.Is(err, inspectFailure) || openCalls != 1 {
		t.Fatalf("error/calls = %v/%d", err, openCalls)
	}
}

func TestCopyLogsRetriesTransientRunnerInspection(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	terminalPod := pod.DeepCopy()
	terminalPod.Status.Phase = corev1.PodSucceeded
	terminalPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  runnerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	client := fake.NewSimpleClientset(pod)
	getCalls := 0
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("temporary Pod inspection failure"))
		}
		return true, terminalPod, nil
	})
	openCalls := 0
	controller, err := NewController(client, baseConfig(), func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		return io.NopCloser(strings.NewReader("")), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.fetchLog = func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := controller.copyLogs(ctx, "run-1", io.Discard, nil, &logCopyState{}); err != nil {
		t.Fatalf("transient Pod inspection repainted completed log copy red: %v", err)
	}
	if getCalls != 2 || openCalls != 2 {
		t.Fatalf("Pod GET/log open calls = %d/%d, want 2/2", getCalls, openCalls)
	}
}

func TestWaitForPodRetriesTransientList(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
	}}
	client := fake.NewSimpleClientset(pod)
	listCalls := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		if listCalls == 1 {
			return true, nil, apierrors.NewServiceUnavailable("temporary Pod list failure")
		}
		return false, nil, nil
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	found, err := controller.waitForPod(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != pod.Name || listCalls != 2 {
		t.Fatalf("found Pod/list calls = %q/%d, want %q/2", found.Name, listCalls, pod.Name)
	}
}

func TestCopyLogsAcceptsEmptyAuthoritativeLogAfterRunnerTerminates(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		return io.NopCloser(strings.NewReader("")), nil
	}
	controller, err := NewController(fake.NewSimpleClientset(pod), baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.copyLogs(context.Background(), "run-1", io.Discard, nil, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	if openCalls != 2 {
		t.Fatalf("follow/final log calls = %d, want 2", openCalls)
	}
}

func TestCopyLogsRejectsChangedAuthoritativeFinalLog(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	controller, err := NewController(fake.NewSimpleClientset(pod), baseConfig(), func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("stable log")), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.fetchLog = func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("changed log")), nil
	}
	_, err = controller.copyLogs(context.Background(), "run-1", io.Discard, nil, &logCopyState{})
	if !errors.Is(err, errPodLogReplayChanged) {
		t.Fatalf("error = %v, want changed authoritative log", err)
	}
}

func TestWaitRetriesPreStartLogOpenRaceWithoutOverridingSuccessfulJob(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": running.Name,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	openFailure := errors.New("container is waiting to start")
	openCalls := 0
	secondOpen := make(chan struct{})
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return nil, openFailure
		}
		if openCalls == 2 {
			close(secondOpen)
		}
		return io.NopCloser(strings.NewReader("complete log")), nil
	}
	config := baseConfig()
	config.JobTimeout = 2 * time.Second
	client := fake.NewSimpleClientset(running, pod)
	jobWatcher := watch.NewRaceFreeFake()
	watchReady := make(chan struct{})
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		close(watchReady)
		return true, jobWatcher, nil
	})
	go func() {
		<-watchReady
		<-secondOpen
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
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	completion, err := controller.Wait(context.Background(), running.Name, "run-1", &output, nil, nil)
	if err != nil {
		t.Fatalf("successful Job was overridden by pre-start log race: %v", err)
	}
	if !completion.Succeeded || completion.ExitCode != 0 || openCalls != 3 || output.String() != "complete log" {
		t.Fatalf("completion/calls/output = %#v/%d/%q", completion, openCalls, output.String())
	}
}

func TestWaitCancelsJobImmediatelyWhenRetainedLogLimitFails(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1",
		Annotations: map[string]string{jobRunIDAnnotation: "run-1"},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": running.Name,
		}, Annotations: map[string]string{jobRunIDAnnotation: "run-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(running, pod)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	deleted := make(chan struct{})
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-deleted:
		default:
			close(deleted)
		}
		return false, nil, nil
	})
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("log bytes beyond the limit")), nil
	}
	config := baseConfig()
	config.JobTimeout = 2 * time.Second
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := controller.Wait(ctx, running.Name, "run-1", failingLogDestination{}, nil, nil)
		waitDone <- waitErr
	}()
	select {
	case <-deleted:
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-waitDone
		t.Fatal("log-copy failure did not delete the active Job promptly")
	}
	stoppedPod := pod.DeepCopy()
	stoppedPod.Status.Phase = corev1.PodFailed
	stoppedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  runnerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), stoppedPod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if waitErr := <-waitDone; !errors.Is(waitErr, ErrRetainedLogLimit) {
		t.Fatalf("Wait error = %v, want retained-log limit", waitErr)
	}
}

func TestWaitCancelsActiveJobWhenLogReplayChanges(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1",
		Annotations: map[string]string{jobRunIDAnnotation: "run-1"},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": running.Name,
		}, Annotations: map[string]string{jobRunIDAnnotation: "run-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(running, pod)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	deleted := make(chan struct{})
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-deleted:
		default:
			close(deleted)
		}
		return false, nil, nil
	})
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return io.NopCloser(&readThenError{
				body: []byte("stable prefix"), err: errors.New("log stream reset"), failed: make(chan struct{}),
			}), nil
		}
		return io.NopCloser(strings.NewReader("changed prefix")), nil
	}
	config := baseConfig()
	config.JobTimeout = 2 * time.Second
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := controller.Wait(context.Background(), running.Name, "run-1", io.Discard, nil, nil)
		waitDone <- waitErr
	}()
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("log replay mismatch did not delete the active Job promptly")
	}
	stoppedPod := pod.DeepCopy()
	stoppedPod.Status.Phase = corev1.PodFailed
	stoppedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  runnerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), stoppedPod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if waitErr := <-waitDone; !errors.Is(waitErr, errPodLogReplayChanged) {
		t.Fatalf("Wait error = %v, want changed log replay", waitErr)
	}
}

func TestWaitResumesMidstreamLogTransportFailureWithoutDeletingHealthyJob(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{
			"oberth.ci/run": "run-1", "job-name": running.Name,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(running, pod)
	jobWatcher := watch.NewRaceFreeFake()
	watchReady := make(chan struct{})
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		close(watchReady)
		return true, jobWatcher, nil
	})
	deleted := make(chan struct{}, 1)
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted <- struct{}{}
		return false, nil, nil
	})
	logFailure := errors.New("apiserver log stream reset")
	streamFailed := make(chan struct{})
	reconnected := make(chan struct{})
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return io.NopCloser(&readThenError{body: []byte("partial log"), err: logFailure, failed: streamFailed}), nil
		}
		if openCalls == 2 {
			close(reconnected)
		}
		return io.NopCloser(strings.NewReader("partial log completed")), nil
	}
	config := baseConfig()
	config.JobTimeout = 2 * time.Second
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type waitResult struct {
		completion Completion
		err        error
	}
	done := make(chan waitResult, 1)
	go func() {
		completion, waitErr := controller.Wait(context.Background(), running.Name, "run-1", io.Discard, nil, nil)
		done <- waitResult{completion: completion, err: waitErr}
	}()
	<-watchReady
	<-streamFailed
	select {
	case <-deleted:
		t.Fatal("transient source log failure deleted a healthy Job")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("log copier did not reconnect after the transient failure")
	}
	finishedPod := pod.DeepCopy()
	finishedPod.Status.Phase = corev1.PodSucceeded
	finishedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Reason: "Completed", Message: successfulTerminationMessage,
		}},
	}}
	if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), finishedPod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	terminal := running.DeepCopy()
	terminal.ResourceVersion = "2"
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	jobWatcher.Modify(terminal)
	result := <-done
	if !result.completion.Succeeded || result.completion.ExitCode != 0 || result.err != nil || openCalls < 3 {
		t.Fatalf("completion/error = %#v/%v", result.completion, result.err)
	}
	select {
	case <-deleted:
		t.Fatal("completed Job was deleted after source log failure")
	default:
	}
}

func TestCopyLogsRetriesOpenErrorAfterRunnerStarted(t *testing.T) {
	openFailure := errors.New("log transport failed")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	openCalls := 0
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		openCalls++
		if openCalls == 1 {
			return nil, openFailure
		}
		if openCalls == 2 {
			finished := pod.DeepCopy()
			finished.Status.Phase = corev1.PodSucceeded
			finished.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}
			if _, err := client.CoreV1().Pods("oberth").Update(context.Background(), finished, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return io.NopCloser(strings.NewReader("complete log")), nil
	}
	controller, err := NewController(client, baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err = controller.copyLogs(context.Background(), "run-1", &output, nil, &logCopyState{}); err != nil {
		t.Fatal(err)
	}
	if openCalls != 3 || output.String() != "complete log" {
		t.Fatalf("calls/output = %d/%q", openCalls, output.String())
	}
}

func TestTerminalCompletionReadsStructuredTerminationMessage(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "oberth", Labels: map[string]string{"job-name": "ci-example-r-1"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: `[{"burn":"test","step":"go-test","status":"failed","exit_code":1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"}]`}},
		}}},
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-example-r-1", Namespace: "oberth"},
		Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": "ci-example-r-1"}}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}},
	}
	completion, err := controller.completion(context.Background(), &failed, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if completion.Succeeded || completion.ExitCode != 1 || !strings.Contains(string(completion.Summary), `"burn":"test"`) {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestTerminalCompletionRejectsInvalidTerminationMessages(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{name: "object instead of array", message: `{"burn":"test"}`},
		{name: "null instead of array", message: `null`},
		{name: "unknown field", message: `[{"burn":"test","step":"go-test","status":"failed","exit_code":1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z","error":"unsafe"}]`},
		{name: "missing required field", message: `[{"burn":"test","status":"failed","exit_code":1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"}]`},
		{name: "forbidden status", message: `[{"burn":"test","step":"go-test","status":"interrupted","exit_code":-1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"}]`},
		{name: "invalid start time", message: `[{"burn":"test","step":"go-test","status":"failed","exit_code":1,"started_at":"eventually","finished_at":"2026-08-04T10:00:01Z"}]`},
		{name: "finished before start", message: `[{"burn":"test","step":"go-test","status":"failed","exit_code":1,"started_at":"2026-08-04T10:00:02Z","finished_at":"2026-08-04T10:00:01Z"}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "oberth", Labels: map[string]string{"job-name": "ci-example-r-1"}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: test.message}},
				}}},
			})
			controller, err := NewController(client, baseConfig(), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			failed := batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "ci-example-r-1", Namespace: "oberth"},
				Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": "ci-example-r-1"}}},
				Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
			}
			if _, err := controller.completion(context.Background(), &failed, "pod-1"); err == nil {
				t.Fatal("completion accepted invalid termination message")
			}
		})
	}
}

func TestTerminalCompletionRejectsGreenWithoutBindingEvidence(t *testing.T) {
	tests := []struct {
		name    string
		message *string
	}{
		{name: "no terminated runner"},
		{name: "missing summary", message: stringPointer("")},
		{name: "empty summary", message: stringPointer("[]")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if test.message != nil {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "oberth", Labels: map[string]string{"job-name": "ci-example-r-1"}},
					Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
						Name: "run", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", Message: *test.message}},
					}}},
				})
			}
			controller, err := NewController(fake.NewSimpleClientset(objects...), baseConfig(), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			complete := batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "ci-example-r-1", Namespace: "oberth"},
				Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": "ci-example-r-1"}}},
				Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
			}
			if _, err := controller.completion(context.Background(), &complete, ""); err == nil {
				t.Fatal("green completion accepted without binding runner evidence")
			}
		})
	}
}

func TestWaitForTerminalRecoversClosedAndErrorWatches(t *testing.T) {
	tests := []struct {
		name      string
		interrupt func(*watch.RaceFreeFakeWatcher)
	}{
		{name: "closed channel", interrupt: func(watcher *watch.RaceFreeFakeWatcher) { watcher.Stop() }},
		{name: "error event", interrupt: func(watcher *watch.RaceFreeFakeWatcher) {
			watcher.Error(&metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: 410})
		}},
		{name: "deleted event", interrupt: func(watcher *watch.RaceFreeFakeWatcher) {
			watcher.Delete(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth"}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
			client := fake.NewSimpleClientset(running)
			watchCalls := 0
			client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
				watchCalls++
				watcher := watch.NewRaceFreeFake()
				if watchCalls == 1 {
					go test.interrupt(watcher)
				} else {
					terminal := running.DeepCopy()
					terminal.ResourceVersion = "2"
					terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
					go watcher.Modify(terminal)
				}
				return true, watcher, nil
			})
			controller, err := NewController(client, baseConfig(), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			terminal, err := controller.waitForTerminal(ctx, running.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !terminalJob(terminal) || watchCalls != 2 {
				t.Fatalf("terminal = %#v, watch calls = %d", terminal.Status, watchCalls)
			}
		})
	}
}

func TestWaitForTerminalRecoversTransientGet(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	client := fake.NewSimpleClientset(running)
	getCalls := 0
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("temporary apiserver failure"))
		}
		terminal := running.DeepCopy()
		terminal.ResourceVersion = "2"
		terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		return true, terminal, nil
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	terminal, err := controller.waitForTerminal(ctx, running.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !terminalJob(terminal) || getCalls != 2 {
		t.Fatalf("terminal = %#v, GET calls = %d", terminal.Status, getCalls)
	}
}

func TestWaitCancelsJobWhenTerminalObservationFails(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1",
		Annotations: map[string]string{jobRunIDAnnotation: "run-1"},
	}}
	client := fake.NewSimpleClientset(running)
	getCalls := 0
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewForbidden(batchv1.Resource("jobs"), running.Name, errors.New("forbidden"))
		}
		return false, nil, nil
	})
	deleted := false
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})
	config := baseConfig()
	config.JobTimeout = time.Second
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.Wait(context.Background(), running.Name, "run-1", io.Discard, nil, nil)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Wait error = %v, want forbidden", err)
	}
	if !deleted {
		t.Fatal("terminal observation failure left the active Job orphaned")
	}
}

func TestWaitForTerminalPacesWatchRecovery(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	client := fake.NewSimpleClientset(running)
	var watchStarted []time.Time
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchStarted = append(watchStarted, time.Now())
		watcher := watch.NewRaceFreeFake()
		if len(watchStarted) == 1 {
			go watcher.Stop()
		} else {
			terminal := running.DeepCopy()
			terminal.ResourceVersion = "2"
			terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			go watcher.Modify(terminal)
		}
		return true, watcher, nil
	})
	controller, err := NewController(client, baseConfig(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := controller.waitForTerminal(ctx, running.Name); err != nil {
		t.Fatal(err)
	}
	if len(watchStarted) != 2 {
		t.Fatalf("watch starts = %d, want 2", len(watchStarted))
	}
	if elapsed := watchStarted[1].Sub(watchStarted[0]); elapsed < 90*time.Millisecond {
		t.Fatalf("watch recovered after %s, want at least 90ms", elapsed)
	}
}

func TestRetryDelayIsBoundedExponential(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 100 * time.Millisecond},
		{failures: 2, want: 200 * time.Millisecond},
		{failures: 3, want: 400 * time.Millisecond},
		{failures: 7, want: 5 * time.Second},
		{failures: 100, want: 5 * time.Second},
	}
	for _, test := range tests {
		if got := retryDelay(test.failures); got != test.want {
			t.Fatalf("retryDelay(%d) = %s, want %s", test.failures, got, test.want)
		}
	}
}

func TestEmptyStreamRetryDelayStaysBelowLogCompletionGrace(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 100 * time.Millisecond},
		{failures: 2, want: 200 * time.Millisecond},
		{failures: 3, want: 400 * time.Millisecond},
		{failures: 4, want: 800 * time.Millisecond},
		{failures: 5, want: time.Second},
		{failures: 100, want: time.Second},
	}
	for _, test := range tests {
		got := emptyStreamRetryDelay(test.failures)
		if got != test.want || got >= logCompletionGrace {
			t.Fatalf("emptyStreamRetryDelay(%d) = %s, want %s below %s", test.failures, got, test.want, logCompletionGrace)
		}
	}
}

func TestWaitUsesBoundedOuterDeadline(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	client := fake.NewSimpleClientset(running)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	config := baseConfig()
	config.JobTimeout = 40 * time.Millisecond
	controller, err := NewController(client, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = controller.Wait(context.Background(), running.Name, "run-1", nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Wait returned after %s", elapsed)
	}
}

func TestWaitDeadlineBoundsPostTerminalLogGrace(t *testing.T) {
	terminal := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
	}}
	config := baseConfig()
	config.JobTimeout = 40 * time.Millisecond
	streamer := func(ctx context.Context, _, _ string) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	controller, err := NewController(fake.NewSimpleClientset(terminal, pod), config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = controller.Wait(context.Background(), terminal.Name, "run-1", nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Wait returned after %s", elapsed)
	}
}

func TestWaitRejectsGreenCompletionWhenAuthoritativeLogGraceExpires(t *testing.T) {
	terminal := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
	}}
	streamer := func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	controller, err := NewController(fake.NewSimpleClientset(terminal, pod), baseConfig(), streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.logCompletionGrace = 20 * time.Millisecond
	controller.fetchLog = func(ctx context.Context, _, _ string) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	completion, err := controller.Wait(context.Background(), terminal.Name, "run-1", nil, nil, nil)
	if !errors.Is(err, errIncompleteFinalLog) {
		t.Fatalf("error = %v, want incomplete authoritative final log", err)
	}
	if completion.Succeeded {
		t.Fatalf("completion = %#v, want non-green result", completion)
	}
}

func TestWaitJoinsLogCopyBeforeReturningOnCancellation(t *testing.T) {
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ci-demo-run", Namespace: "oberth", ResourceVersion: "1"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ci-demo-run-pod", Namespace: "oberth", Labels: map[string]string{"oberth.ci/run": "run-1"},
	}}
	client := fake.NewSimpleClientset(running, pod)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	stream := newCloseDrivenReader()
	destination := newWriteSignal()
	streamer := func(context.Context, string, string) (io.ReadCloser, error) { return stream, nil }
	config := baseConfig()
	config.JobTimeout = time.Second
	controller, err := NewController(client, config, streamer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := controller.Wait(ctx, running.Name, "run-1", destination, nil, nil)
		waitDone <- waitErr
	}()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("log stream did not start")
	}
	cancel()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancellation")
	}
	joinedAtReturn := false
	select {
	case <-destination.written:
		joinedAtReturn = true
	default:
	}
	_ = stream.Close()
	select {
	case <-destination.written:
	case <-time.After(time.Second):
		t.Fatal("log copier did not finish during test cleanup")
	}
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", waitErr)
	}
	if !joinedAtReturn {
		t.Fatal("Wait returned before the log copier finished")
	}
}

type failingLogDestination struct{}

func (failingLogDestination) Write([]byte) (int, error) { return 0, ErrRetainedLogLimit }

type closeSignalReader struct {
	closed chan struct{}
	once   sync.Once
}

func (*closeSignalReader) Read([]byte) (int, error) { return 0, io.EOF }

func (reader *closeSignalReader) Close() error {
	reader.once.Do(func() { close(reader.closed) })
	return nil
}

type eofAfterSignalReader struct {
	body   []byte
	signal <-chan struct{}
}

func (reader *eofAfterSignalReader) Read(destination []byte) (int, error) {
	if len(reader.body) > 0 {
		written := copy(destination, reader.body)
		reader.body = reader.body[written:]
		return written, nil
	}
	<-reader.signal
	return 0, io.EOF
}

func waitForPodProgress(t *testing.T, client *fake.Clientset, podName string, want podProgress) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pod, err := client.CoreV1().Pods("oberth").Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pod.Annotations["oberth.ci/current-burn"] == want.burn && pod.Annotations["oberth.ci/current-step"] == want.step {
			return pod
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pod progress = %#v/%#v, want %#v", pod.Labels, pod.Annotations, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type blockingPodPatchClient struct {
	kubernetes.Interface
	pod      *corev1.Pod
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (client *blockingPodPatchClient) CoreV1() typedcorev1.CoreV1Interface {
	return &blockingPodPatchCore{
		CoreV1Interface: client.Interface.CoreV1(), pod: client.pod,
		started: client.started, release: client.release, returned: client.returned,
	}
}

type blockingPodPatchCore struct {
	typedcorev1.CoreV1Interface
	pod      *corev1.Pod
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (client *blockingPodPatchCore) Pods(namespace string) typedcorev1.PodInterface {
	return &blockingPodPatchPods{
		PodInterface: client.CoreV1Interface.Pods(namespace), pod: client.pod,
		started: client.started, release: client.release, returned: client.returned,
	}
}

type blockingPodPatchPods struct {
	typedcorev1.PodInterface
	pod      *corev1.Pod
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (pods *blockingPodPatchPods) Patch(
	ctx context.Context,
	_ string,
	_ types.PatchType,
	_ []byte,
	_ metav1.PatchOptions,
	_ ...string,
) (*corev1.Pod, error) {
	close(pods.started)
	defer close(pods.returned)
	select {
	case <-pods.release:
		return pods.pod.DeepCopy(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type readThenError struct {
	body   []byte
	err    error
	failed chan struct{}
	once   sync.Once
}

func (reader *readThenError) Read(destination []byte) (int, error) {
	if len(reader.body) != 0 {
		written := copy(destination, reader.body)
		reader.body = reader.body[written:]
		return written, nil
	}
	reader.once.Do(func() { close(reader.failed) })
	return 0, reader.err
}

type closeDrivenReader struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newCloseDrivenReader() *closeDrivenReader {
	return &closeDrivenReader{started: make(chan struct{}), closed: make(chan struct{})}
}

func (reader *closeDrivenReader) Read(buffer []byte) (int, error) {
	reader.startOnce.Do(func() { close(reader.started) })
	<-reader.closed
	time.Sleep(20 * time.Millisecond)
	return copy(buffer, "late log output"), io.EOF
}

func (reader *closeDrivenReader) Close() error {
	reader.closeOnce.Do(func() { close(reader.closed) })
	return nil
}

type writeSignal struct {
	written chan struct{}
	once    sync.Once
}

type chunkReader struct {
	chunks [][]byte
}

func (reader *chunkReader) Read(buffer []byte) (int, error) {
	if len(reader.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := reader.chunks[0]
	reader.chunks = reader.chunks[1:]
	return copy(buffer, chunk), nil
}

func newWriteSignal() *writeSignal { return &writeSignal{written: make(chan struct{})} }

func stringPointer(value string) *string { return &value }

func (writer *writeSignal) Write(value []byte) (int, error) {
	writer.once.Do(func() { close(writer.written) })
	return len(value), nil
}
