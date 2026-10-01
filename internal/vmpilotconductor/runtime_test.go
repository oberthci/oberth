package vmpilotconductor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type fixture struct {
	ctx    context.Context
	s      *store.Store
	client *dynamicfake.FakeDynamicClient
	b      *Backend
	plan   vmrunner.PilotPlan
	now    time.Time
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureAt(t, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
}

func newFixtureAt(t *testing.T, now time.Time) *fixture {
	t.Helper()
	f := &fixture{ctx: context.Background(), now: now}
	var err error
	f.s, err = store.Open(f.ctx, filepath.Join(t.TempDir(), "journal.sqlite"), store.Options{Now: func() time.Time { return f.now }})
	must(t, err)
	t.Cleanup(func() { must(t, f.s.Close()) })
	upstream, err := f.s.CreateUpstream(f.ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	must(t, err)
	repo, err := f.s.CreateRepository(f.ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	must(t, err)
	_, err = f.s.EnqueueRun(f.ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "pilot", SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "branch"})
	must(t, err)
	run, err := f.s.ClaimNextRun(f.ctx)
	must(t, err)
	f.plan = vmrunner.PilotPlan{Version: 1, Profile: vmrunner.BeaconTransportProfile,
		Spec:              vmrunner.VMRunSpec{RunID: run.ID, Repo: upstream.QualifiedRepo(repo.Name), CandidateSHA: run.TestedSHA, SuiteRevision: strings.Repeat("b", 40), GuestImageRef: "registry.example/guest@sha256:" + strings.Repeat("c", 64), Resources: vmrunner.VMResources{CPUCores: 1, MemoryMiB: 512}, Deadline: 5 * time.Minute},
		ConductorImageRef: "registry.example/conductor@sha256:" + strings.Repeat("e", 64), KernelDigest: "sha256:" + strings.Repeat("1", 64), InitramfsDigest: "sha256:" + strings.Repeat("2", 64), GuestHelperDigest: "sha256:" + strings.Repeat("3", 64), ArtifactDigest: "sha256:" + strings.Repeat("4", 64), ArtifactBytes: 1024,
		GuestNamespace: "pilot-guest", ConductorNamespace: "pilot-conductor", ServerNamespace: "oberth"}
	_, err = f.s.ReservePilot(f.ctx, f.plan)
	must(t, err)
	must(t, f.s.SubmitPilot(f.ctx, f.plan.Spec.RunID))
	f.now = f.now.Add(time.Second)
	f.client = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{jobs: "JobList", pods: "PodList"})
	f.b, err = New(f.client, f.s, Config{ServiceAccount: "pilot-conductor", RequestTimeout: time.Second})
	must(t, err)
	f.client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		// API observation: the durable submit must already exist before create.
		resources, err := f.s.PilotResources(f.ctx, f.plan.Spec.RunID)
		must(t, err)
		if len(resources) != 1 || !resources[0].Submitted || resources[0].Receipt.UID != "" {
			t.Fatal("create preceded durable submit")
		}
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		var job batchv1.Job
		must(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &job))
		job.UID = "job-uid"
		job.CreationTimestamp = metav1.NewTime(f.now)
		job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": "job-uid"}}
		job.Spec.Template.Labels["batch.kubernetes.io/controller-uid"] = "job-uid"
		job.Spec.Template.Labels["batch.kubernetes.io/job-name"] = job.Name
		job.Spec.Template.Spec.DeprecatedServiceAccount = job.Spec.Template.Spec.ServiceAccountName //nolint:staticcheck // Simulate the API's legacy ServiceAccount alias.
		job.Spec.Template.Spec.PreemptionPolicy = ptr(corev1.PreemptLowerPriority)
		object = toObject(t, &job)
		return true, object, f.client.Tracker().Create(jobs, object, job.Namespace)
	})
	return f
}

func toObject(t *testing.T, value any) *unstructured.Unstructured {
	t.Helper()
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	must(t, err)
	return &unstructured.Unstructured{Object: object}
}

func (f *fixture) create(t *testing.T) vmrunner.PilotResource {
	t.Helper()
	_, err := f.b.Create(f.ctx, f.plan)
	must(t, err)
	return f.owned(t, vmrunner.ConductorResource)
}

func (f *fixture) owned(t *testing.T, key string) vmrunner.PilotResource {
	t.Helper()
	resources, err := f.s.PilotResources(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	for _, r := range resources {
		if r.Intent.Key == key {
			return r
		}
	}
	t.Fatalf("missing resource %s", key)
	return vmrunner.PilotResource{}
}

func (f *fixture) pod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	job := f.owned(t, vmrunner.ConductorResource)
	object, err := f.client.Resource(jobs).Namespace(f.plan.ConductorNamespace).Get(f.ctx, job.Intent.Name, metav1.GetOptions{})
	must(t, err)
	var actual batchv1.Job
	must(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &actual))
	f.now = f.now.Add(time.Second)
	pod := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.plan.ConductorNamespace, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(f.now), Labels: actual.Spec.Template.Labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: actual.Name, UID: actual.UID, Controller: ptr(true), BlockOwnerDeletion: ptr(true)}}, Finalizers: []string{batchv1.JobTrackingFinalizer}}, Spec: actual.Spec.Template.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "conductor", Image: f.plan.ConductorImageRef, ImageID: f.plan.ConductorImageRef, ContainerID: "containerd://" + name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(f.now)}}}}}}
	pod.Spec.NodeName = "worker-1"
	pod.Spec.Priority = ptr[int32](0)
	pod.Spec.Tolerations = []corev1.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr[int64](300)},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr[int64](300)},
	}
	must(t, f.client.Tracker().Create(pods, toObject(t, pod), pod.Namespace))
	return pod
}

func (f *fixture) savePod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	must(t, f.client.Tracker().Update(pods, toObject(t, pod), pod.Namespace))
}

func (f *fixture) terminated(t *testing.T, pod *corev1.Pod, code int32) {
	t.Helper()
	status := &pod.Status.ContainerStatuses[0]
	started := status.State.Running.StartedAt
	f.now = f.now.Add(time.Second)
	reason := "Completed"
	if code != 0 {
		reason = "Error"
	}
	status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, StartedAt: started, FinishedAt: metav1.NewTime(f.now), ContainerID: status.ContainerID}}
	pod.Status.Phase = corev1.PodSucceeded // Even a misleading phase cannot change exit7.
	f.savePod(t, pod)
}

func countActions(client *dynamicfake.FakeDynamicClient, verb string) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

func TestCreateClosedJobAndSoleSubmission(t *testing.T) {
	f := newFixture(t)
	owned := f.create(t)
	object, err := f.client.Resource(jobs).Namespace(f.plan.ConductorNamespace).Get(f.ctx, owned.Intent.Name, metav1.GetOptions{})
	must(t, err)
	var job batchv1.Job
	must(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &job))
	spec := job.Spec.Template.Spec
	if *job.Spec.BackoffLimit != 0 || *job.Spec.Parallelism != 1 || *job.Spec.Completions != 1 || *job.Spec.ActiveDeadlineSeconds != 300 || *job.Spec.PodReplacementPolicy != batchv1.Failed || spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("retry or deadline recipe widened")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks || len(spec.Volumes) != 0 || len(spec.InitContainers) != 0 || len(spec.Containers) != 1 || spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Fatal("ambient authority in recipe")
	}
	c := spec.Containers[0]
	if c.Image != f.plan.ConductorImageRef || len(c.Command) != 1 || c.Command[0] != "/usr/local/bin/beacon-conductor" || len(c.Args) != 0 || len(c.Env) != 0 || len(c.EnvFrom) != 0 || len(c.VolumeMounts) != 0 || !c.Stdin || !c.StdinOnce || c.TTY || c.Resources.Limits.Cpu().String() != "1" || c.Resources.Limits.Memory().String() != "256Mi" {
		t.Fatal("conductor process recipe changed")
	}
	if *spec.SecurityContext.RunAsUser != 65534 || !*spec.SecurityContext.RunAsNonRoot || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("security recipe changed")
	}
	_, err = f.b.Create(f.ctx, f.plan)
	must(t, err)
	if countActions(f.client, "create") != 1 {
		t.Fatal("retry issued another create")
	}
}

type cleanupBeforeSubmit struct{ vmrunner.PilotJournal }

func (j cleanupBeforeSubmit) SubmitPilotResource(ctx context.Context, runID, key string) error {
	if err := j.BeginPilotCleanup(ctx, runID); err != nil {
		return err
	}
	return j.PilotJournal.SubmitPilotResource(ctx, runID, key)
}

func TestCleanupWinningSubmitPreventsCreate(t *testing.T) {
	f := newFixture(t)
	f.b.journal = cleanupBeforeSubmit{PilotJournal: f.s}
	if _, err := f.b.Create(f.ctx, f.plan); err == nil {
		t.Fatal("cleanup race created conductor")
	}
	if countActions(f.client, "create") != 0 {
		t.Fatal("submit refusal reached API")
	}
}

func TestAmbiguousCreateNeverRetriesAndCanBindLate(t *testing.T) {
	f := newFixture(t)
	f.client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTimeoutError("ambiguous", 1)
	})
	if _, err := f.b.Create(f.ctx, f.plan); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := f.b.Create(f.ctx, f.plan); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
		t.Fatalf("retry: %v", err)
	}
	if countActions(f.client, "create") != 1 {
		t.Fatal("ambiguous create repeated")
	}
	owned := f.owned(t, vmrunner.ConductorResource)
	if !owned.Submitted || owned.Rejected || owned.Receipt.UID != "" {
		t.Fatal("ambiguous obligation lost")
	}
	reconciler := vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}
	if err := reconciler.Cleanup(f.ctx, f.plan.Spec.RunID); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
		t.Fatalf("unbound absence released: %v", err)
	}
	state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	if state.Cleaned {
		t.Fatal("ambiguous notfound discharged obligation")
	}
	// Simulate the original timed-out create becoming visible during cleanup.
	job, _, err := f.b.job(f.plan)
	must(t, err)
	job.UID = "late-uid"
	job.CreationTimestamp = metav1.NewTime(f.now)
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": "late-uid"}}
	job.Spec.Template.Labels = map[string]string{identityLabel: job.Labels[identityLabel], "batch.kubernetes.io/controller-uid": "late-uid", "batch.kubernetes.io/job-name": job.Name}
	must(t, f.client.Tracker().Create(jobs, toObject(t, job), job.Namespace))
	must(t, reconciler.Cleanup(f.ctx, f.plan.Spec.RunID))
	if f.owned(t, vmrunner.ConductorResource).Receipt.UID != "late-uid" {
		t.Fatal("late UID not durably bound")
	}
}

func TestActualPodAuthorityMutationsRefusedAndRetained(t *testing.T) {
	attacks := map[string]func(*corev1.Pod){
		"token": func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr(true) },
		"volume": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "credential"}}}}
		},
		"env":     func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "OVERRIDE", Value: "attack"}} },
		"command": func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/sh"} },
		"sidecar": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "extra", Image: "untrusted"})
		},
		"privileged":       func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr(true) },
		"host-network":     func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"annotation":       func(p *corev1.Pod) { p.Annotations = map[string]string{"claimed-spec": "unchanged"} },
		"wrong-image":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("f", 64) },
		"digest-substring": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = fakeImageSuffix() },
		"restart":          func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"hidden-previous": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0}
		},
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.create(t)
			pod := f.pod(t, "first")
			attack(pod)
			f.savePod(t, pod)
			if _, err := f.b.BindAttempt(f.ctx, f.plan); err == nil || errors.Is(err, ErrProcessPending) {
				t.Fatalf("accepted mutation: %v", err)
			}
			state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if state.Attempt != nil || state.Failure == "" {
				t.Fatal("unsafe attempt not durably refused")
			}
			resources, err := f.s.PilotResources(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if len(resources) != 2 {
				t.Fatal("unsafe owned Pod cleanup obligation lost")
			}
			// Cleanup observes and discharges two durable resources through
			// multiple API and journal calls. Keep this success path bounded
			// without making a loaded release test depend on a one-second budget.
			must(t, (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: 10 * time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID))
			state, err = f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if !state.Cleaned || state.Failure == "" {
				t.Fatal("unsafe attempt refusal was not retained through cleanup")
			}
		})
	}
}

func fakeImageSuffix() string { return "untrusted@sha256:" + strings.Repeat("e", 64) + "-suffix" }

func TestActualExitAndDurableAttempt(t *testing.T) {
	for _, code := range []int32{0, 7} {
		t.Run(string('0'+code), func(t *testing.T) {
			f := newFixture(t)
			f.create(t)
			pod := f.pod(t, "first")
			attempt, err := f.b.BindAttempt(f.ctx, f.plan)
			must(t, err)
			if attempt.JobUID != "job-uid" || attempt.PodUID != "first-uid" || attempt.PodName != "first" || attempt.NodeName != "worker-1" ||
				attempt.ContainerID != "containerd://first" || attempt.ImageDigest != "sha256:"+strings.Repeat("e", 64) {
				t.Fatal("attempt not from actual API fields")
			}
			pod.Status.Phase = corev1.PodSucceeded
			f.savePod(t, pod)
			if _, err := f.b.Termination(f.ctx, f.plan, attempt); !errors.Is(err, ErrProcessPending) {
				t.Fatalf("phase substituted for exit: %v", err)
			}
			f.terminated(t, pod, code)
			terminal, err := f.b.Termination(f.ctx, f.plan, attempt)
			if terminal.ExitCode != code || (code == 0 && err != nil) || (code != 0 && err == nil) {
				t.Fatalf("actual exit=%d: %#v %v", code, terminal, err)
			}
			state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if state.Receipt != nil || (code != 0 && state.Failure == "") {
				t.Fatal("exit became suite receipt or failure was lost")
			}
		})
	}
}

func TestReplacementPodAndContainerCannotRebind(t *testing.T) {
	for _, mode := range []string{"second-pod", "container-id", "started-at", "status-disappeared", "pod-disappeared"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.create(t)
			pod := f.pod(t, "first")
			bound, err := f.b.BindAttempt(f.ctx, f.plan)
			must(t, err)
			switch mode {
			case "second-pod":
				f.pod(t, "second")
			case "container-id":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
				f.savePod(t, pod)
			case "started-at":
				pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(f.now.Add(time.Second))
				f.now = f.now.Add(time.Second)
				f.savePod(t, pod)
			case "status-disappeared":
				pod.Status.ContainerStatuses = nil
				f.savePod(t, pod)
			case "pod-disappeared":
				must(t, f.client.Tracker().Delete(pods, pod.Namespace, pod.Name))
			}
			if _, err := f.b.BindAttempt(f.ctx, f.plan); err == nil || errors.Is(err, ErrProcessPending) {
				t.Fatalf("replacement accepted: %v", err)
			}
			state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if state.Failure == "" || state.Attempt == nil || vmrunner.ConductorAttemptIdentity(*state.Attempt) != vmrunner.ConductorAttemptIdentity(bound) {
				t.Fatal("durable original attempt lost")
			}
			if mode == "second-pod" {
				resources, err := f.s.PilotResources(f.ctx, f.plan.Spec.RunID)
				must(t, err)
				if len(resources) != 3 {
					t.Fatal("replacement child not retained")
				}
			}
		})
	}
}

func TestCleanupAcknowledgementAndUIDPreconditions(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	f.pod(t, "first")
	_, err := f.b.BindAttempt(f.ctx, f.plan)
	must(t, err)
	ackOnly := true
	f.client.PrependReactor("delete", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		opts := action.(ktesting.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID == "" || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatal("delete lacked UID scope")
		}
		if ackOnly {
			// The API acknowledges foreground deletion by marking the object;
			// this is still present, and recovery must continue after this state.
			object, err := f.client.Tracker().Get(action.GetResource(), action.GetNamespace(), action.(ktesting.DeleteAction).GetName())
			must(t, err)
			marked := object.(*unstructured.Unstructured).DeepCopy()
			marked.SetDeletionTimestamp(ptr(metav1.NewTime(f.now)))
			finalizers := marked.GetFinalizers()
			found := false
			for _, value := range finalizers {
				if value == metav1.FinalizerDeleteDependents {
					found = true
				}
			}
			if !found {
				marked.SetFinalizers(append(finalizers, metav1.FinalizerDeleteDependents))
			}
			must(t, f.client.Tracker().Update(action.GetResource(), marked, action.GetNamespace()))
			return true, nil, nil
		}
		return false, nil, nil
	})
	r := vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}
	if err := r.Cleanup(f.ctx, f.plan.Spec.RunID); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
		t.Fatalf("ack accepted as absence: %v", err)
	}
	state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	if state.Cleaned {
		t.Fatal("ack released reservation")
	}
	ackOnly = false
	must(t, r.Cleanup(f.ctx, f.plan.Spec.RunID))
	state, err = f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	if !state.Cleaned {
		t.Fatal("actual absence did not settle")
	}
	if _, err := f.s.CompletedPilotReceipt(f.ctx, f.plan.Spec.RunID); err == nil {
		t.Fatal("cleanup became suite success")
	}
}

func TestAbsentJobStillFindsRelabelledOwnedPod(t *testing.T) {
	f := newFixture(t)
	job := f.create(t)
	pod := f.pod(t, "late")
	pod.Labels = map[string]string{"foreign": "label"}
	f.savePod(t, pod)
	must(t, f.client.Tracker().Delete(jobs, job.Intent.Namespace, job.Intent.Name))
	observation, err := f.b.Observe(f.ctx, f.plan, job)
	must(t, err)
	if !observation.Absent || len(observation.Pods) != 1 || observation.Pods[0].Receipt.UID != "late-uid" {
		t.Fatal("surviving owner-UID child hidden by parent absence/labels")
	}
	// This is an ownership/discovery success assertion over a real durable
	// journal, not a one-second filesystem/scheduler benchmark. Keep a finite
	// budget matching the other multi-resource success cleanup above; the
	// deadline-retention contract is exercised independently below.
	must(t, (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: 10 * time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID))
	if _, err := f.client.Resource(pods).Namespace(pod.Namespace).Get(f.ctx, pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("surviving Pod: %v", err)
	}
}

type deadlinePilotJournal struct {
	vmrunner.PilotJournal
	reached bool
}

func (journal *deadlinePilotJournal) PilotExecution(ctx context.Context, _ string) (vmrunner.PilotExecution, error) {
	journal.reached = true
	// Wait for the reconciler's actual deadline. No sleep, synthetic returned
	// deadline error, or timing assertion stands in for the bounded context.
	<-ctx.Done()
	return vmrunner.PilotExecution{}, ctx.Err()
}

func TestCleanupDeadlineRetainsOwnedResources(t *testing.T) {
	f := newFixture(t)
	owned := f.create(t)
	pod := f.pod(t, "survivor")
	journal := &deadlinePilotJournal{PilotJournal: f.s}
	beforeDeletes := countActions(f.client, "delete")
	err := (&vmrunner.PilotReconciler{Journal: journal, Runtime: f.b, Timeout: time.Millisecond}).Cleanup(f.ctx, f.plan.Spec.RunID)
	if !journal.reached || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not propagate its own expired budget: %v", err)
	}
	state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	if state.Cleaned || countActions(f.client, "delete") != beforeDeletes {
		t.Fatal("expired cleanup discharged ownership or deleted a resource")
	}
	retained := f.owned(t, vmrunner.ConductorResource)
	if retained.Cleaned || retained.Receipt != owned.Receipt {
		t.Fatal("deadline changed the durable UID obligation")
	}
	actual, err := f.client.Resource(pods).Namespace(pod.Namespace).Get(f.ctx, pod.Name, metav1.GetOptions{})
	must(t, err)
	if actual.GetUID() != pod.UID {
		t.Fatal("deadline replaced or removed the owned Pod")
	}
	if _, err := f.s.CompletedPilotReceipt(f.ctx, f.plan.Spec.RunID); err == nil {
		t.Fatal("deadline became successful suite evidence")
	}
}

func TestCleanupReplacementAndAPIFailuresRetainObligation(t *testing.T) {
	for _, mode := range []string{"replacement", "get-failure", "list-failure", "incomplete-list"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			job := f.create(t)
			switch mode {
			case "replacement":
				object, err := f.client.Resource(jobs).Namespace(job.Intent.Namespace).Get(f.ctx, job.Intent.Name, metav1.GetOptions{})
				must(t, err)
				object.SetUID("foreign-uid")
				must(t, f.client.Tracker().Update(jobs, object, job.Intent.Namespace))
			case "get-failure":
				f.client.PrependReactor("get", "jobs", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
			case "list-failure":
				f.client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
			case "incomplete-list":
				f.client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"continue": "more"}}}, nil
				})
			}
			if err := (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID); err == nil {
				t.Fatal("incomplete/foreign observation accepted")
			}
			state, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, err)
			if state.Cleaned || countActions(f.client, "delete") != 0 {
				t.Fatal("failure discharged ownership or deleted foreign UID")
			}
		})
	}
}

func TestActualJobSpecCannotUseClaimedIdentity(t *testing.T) {
	for _, mode := range []string{"token", "retry", "command", "image", "extra-field", "annotation", "old-creation", "unknown-finalizer", "premature-foreground-finalizer"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			create := f.client.ReactionChain[0]
			f.client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
				handled, value, err := create.React(action)
				if err != nil {
					return handled, value, err
				}
				object := value.(*unstructured.Unstructured).DeepCopy()
				switch mode {
				case "token":
					must(t, unstructured.SetNestedField(object.Object, true, "spec", "template", "spec", "automountServiceAccountToken"))
				case "retry":
					must(t, unstructured.SetNestedField(object.Object, int64(1), "spec", "backoffLimit"))
				case "command", "image":
					containers, _, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
					must(t, err)
					container := containers[0].(map[string]any)
					if mode == "command" {
						container["command"] = []any{"/bin/sh"}
					} else {
						container["image"] = "registry.example/conductor:latest"
					}
					must(t, unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers"))
				case "extra-field":
					must(t, unstructured.SetNestedField(object.Object, true, "spec", "template", "spec", "futureAuthority"))
				case "annotation":
					object.SetAnnotations(map[string]string{"oberth.ci/spec-identity": f.owned(t, vmrunner.ConductorResource).Intent.SpecIdentity})
				case "old-creation":
					object.SetCreationTimestamp(metav1.NewTime(f.now.Add(-time.Hour)))
				case "unknown-finalizer":
					object.SetDeletionTimestamp(ptr(metav1.NewTime(f.now)))
					object.SetFinalizers([]string{"unreviewed.example/finalizer"})
				case "premature-foreground-finalizer":
					object.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
				}
				must(t, f.client.Tracker().Update(jobs, object, object.GetNamespace()))
				return true, object, nil
			})
			if _, err := f.b.Create(f.ctx, f.plan); err == nil {
				t.Fatal("mutated actual Job accepted")
			}
			resource := f.owned(t, vmrunner.ConductorResource)
			if !resource.Submitted || resource.Rejected || resource.Receipt.UID != "" {
				t.Fatal("mutation lost ambiguous ownership obligation")
			}
			if err := (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID); err == nil {
				t.Fatal("invalid Job released cleanup capacity")
			}
			if countActions(f.client, "delete") != 0 {
				t.Fatal("unbound actual Job deleted")
			}
		})
	}
}

func TestForeignOwnerCannotSupplyAttemptOrCleanupTarget(t *testing.T) {
	f := newFixture(t)
	job := f.create(t)
	pod := f.pod(t, "foreign")
	pod.OwnerReferences[0].UID = "foreign-job-uid"
	f.savePod(t, pod)
	if _, err := f.b.BindAttempt(f.ctx, f.plan); !errors.Is(err, ErrProcessPending) {
		t.Fatalf("foreign child adopted: %v", err)
	}
	observation, err := f.b.Observe(f.ctx, f.plan, job)
	must(t, err)
	if len(observation.Pods) != 0 {
		t.Fatal("label collision adopted foreign child")
	}
	forged := job
	forged.Receipt.UID = "foreign-job-uid"
	if err := f.b.Delete(f.ctx, f.plan, forged); err == nil {
		t.Fatal("caller-selected UID became delete authority")
	}
	if countActions(f.client, "delete") != 0 {
		t.Fatal("forged handle reached deletion API")
	}
}

func TestPendingPodCanScheduleBeforeBinding(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	pod := f.pod(t, "first")
	running := pod.DeepCopy()
	pod.Spec.NodeName = ""
	pod.Status.ContainerStatuses = nil
	f.savePod(t, pod)
	if _, err := f.b.BindAttempt(f.ctx, f.plan); !errors.Is(err, ErrProcessPending) {
		t.Fatalf("pending: %v", err)
	}
	f.savePod(t, running)
	_, err := f.b.BindAttempt(f.ctx, f.plan)
	must(t, err)
}

func TestCleanupDetachesCancellationAndPreservesReplacementPod(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "replacement"}[replacement], func(t *testing.T) {
			f := newFixture(t)
			f.create(t)
			pod := f.pod(t, "first")
			_, err := f.b.BindAttempt(f.ctx, f.plan)
			must(t, err)
			if replacement {
				pod.UID = "other-pod-uid"
				pod.OwnerReferences[0].UID = "foreign-job-uid"
				f.savePod(t, pod)
			}
			ctx, cancel := context.WithCancel(f.ctx)
			cancel()
			err = (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(ctx, f.plan.Spec.RunID)
			if replacement && err == nil {
				t.Fatal("replacement Pod discharged original UID")
			}
			if !replacement {
				must(t, err)
			}
			state, readErr := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
			must(t, readErr)
			if state.Failure == "" || state.Cleaned == replacement {
				t.Fatal("cancellation/ownership cleanup state incorrect")
			}
			if replacement {
				for _, action := range f.client.Actions() {
					if action.GetVerb() == "delete" && action.GetResource() == pods {
						t.Fatal("replacement Pod deleted")
					}
				}
			}
		})
	}
}

func TestSerializedSameSecondAdmissionAndRecovery(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	for _, mode := range []string{"lifecycle", "create-response-lost"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixtureAt(t, base.Add(250*time.Millisecond))
			f.now = base.Add(500 * time.Millisecond)
			if mode == "create-response-lost" {
				create := f.client.ReactionChain[0]
				f.client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
					_, _, err := create.React(action)
					if err != nil {
						return true, nil, err
					}
					return true, nil, apierrors.NewTimeoutError("response lost after create", 1)
				})
				if _, err := f.b.Create(f.ctx, f.plan); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
					t.Fatalf("ambiguous response: %v", err)
				}
				must(t, (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID))
				owned := f.owned(t, vmrunner.ConductorResource)
				if !owned.Cleaned || owned.Receipt.UID != "job-uid" || !owned.Receipt.CreatedAt.Equal(base) {
					t.Fatal("recovery lost actual second-precision creation receipt")
				}
				return
			}
			job := f.create(t)
			if !job.Receipt.CreatedAt.Equal(base) {
				t.Fatal("fake API did not serialize Job timestamp at whole-second precision")
			}
			// pod() advances its supplied host clock by one second. Both actual Pod
			// creation/start are .750s in the admission second, serialized to .000s.
			f.now = base.Add(-250 * time.Millisecond)
			pod := f.pod(t, "same-second")
			attempt, err := f.b.BindAttempt(f.ctx, f.plan)
			must(t, err)
			if !attempt.StartedAt.Equal(base) || attempt.StartedAt.Nanosecond() != 0 {
				t.Fatal("observed process timestamp was fabricated or rounded up")
			}
			f.terminated(t, pod, 0)
			_, err = f.b.Termination(f.ctx, f.plan, attempt)
			must(t, err)
			// Same-name process replacement remains forbidden after the precision fix.
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
			pod.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://replacement"
			f.savePod(t, pod)
			if _, err := f.b.Termination(f.ctx, f.plan, attempt); err == nil {
				t.Fatal("replacement accepted at same timestamp")
			}
			must(t, (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID))
		})
	}
}

func TestSerializedEarlierSecondJobStillRejected(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	f := newFixtureAt(t, base.Add(250*time.Millisecond))
	create := f.client.ReactionChain[0]
	f.client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		_, value, err := create.React(action)
		if err != nil {
			return true, value, err
		}
		object := value.(*unstructured.Unstructured).DeepCopy()
		// The API's serialized value is one genuinely earlier second.
		object.SetCreationTimestamp(metav1.NewTime(base.Add(-500 * time.Millisecond)))
		var job batchv1.Job
		must(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &job))
		object = toObject(t, &job)
		must(t, f.client.Tracker().Update(jobs, object, object.GetNamespace()))
		return true, object, nil
	})
	if _, err := f.b.Create(f.ctx, f.plan); err == nil {
		t.Fatal("earlier-second Job admitted")
	}
	if err := (&vmrunner.PilotReconciler{Journal: f.s, Runtime: f.b, Timeout: time.Second}).Cleanup(f.ctx, f.plan.Spec.RunID); err == nil {
		t.Fatal("earlier-second Job bound by recovery")
	}
	if countActions(f.client, "delete") != 0 {
		t.Fatal("unowned older Job deleted")
	}
}
