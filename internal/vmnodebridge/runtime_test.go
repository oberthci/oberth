package vmnodebridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/oberthci/oberth/internal/vmrunner"
)

type fakeCRI struct {
	runtimev1.RuntimeServiceClient
	status                                *runtimev1.ContainerStatus
	item                                  *runtimev1.Container
	sandbox                               *runtimev1.PodSandboxStatus
	attach                                int
	change                                func()
	statusID, listID, sandboxID, attachID string
	attachShape                           *runtimev1.AttachRequest
}

func (fake *fakeCRI) ContainerStatus(_ context.Context, request *runtimev1.ContainerStatusRequest, _ ...grpc.CallOption) (*runtimev1.ContainerStatusResponse, error) {
	fake.statusID = request.GetContainerId()
	return &runtimev1.ContainerStatusResponse{Status: fake.status}, nil
}

func (fake *fakeCRI) ListContainers(_ context.Context, request *runtimev1.ListContainersRequest, _ ...grpc.CallOption) (*runtimev1.ListContainersResponse, error) {
	fake.listID = request.GetFilter().GetId()
	return &runtimev1.ListContainersResponse{Containers: []*runtimev1.Container{fake.item}}, nil
}

func (fake *fakeCRI) PodSandboxStatus(_ context.Context, request *runtimev1.PodSandboxStatusRequest, _ ...grpc.CallOption) (*runtimev1.PodSandboxStatusResponse, error) {
	fake.sandboxID = request.GetPodSandboxId()
	return &runtimev1.PodSandboxStatusResponse{Status: fake.sandbox}, nil
}

func (fake *fakeCRI) Attach(_ context.Context, request *runtimev1.AttachRequest, _ ...grpc.CallOption) (*runtimev1.AttachResponse, error) {
	fake.attach++
	fake.attachID = request.GetContainerId()
	fake.attachShape = request
	if fake.change != nil {
		fake.change()
	}
	return &runtimev1.AttachResponse{Url: "https://127.0.0.1:10010/attach/one-use"}, nil
}

func runtimeFixture(t *testing.T) (AttachRequest, *fakeCRI, *Runtime) {
	t.Helper()
	created := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	started := created.Add(time.Minute)
	digest := "sha256:" + strings.Repeat("e", 64)
	plan := vmrunner.PilotPlan{Version: 1, Profile: vmrunner.BeaconTransportProfile,
		Spec: vmrunner.VMRunSpec{RunID: "run-bridge", Repo: "acme-ebpf", CandidateSHA: strings.Repeat("a", 40),
			SuiteRevision: strings.Repeat("b", 40), GuestImageRef: "registry.example/guest@sha256:" + strings.Repeat("c", 64),
			Resources: vmrunner.VMResources{CPUCores: 1, MemoryMiB: 512}, Deadline: 5 * time.Minute},
		ConductorImageRef: "registry.example/conductor@" + digest, KernelDigest: "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest: "sha256:" + strings.Repeat("2", 64), GuestHelperDigest: "sha256:" + strings.Repeat("3", 64),
		ArtifactDigest: "sha256:" + strings.Repeat("4", 64), ArtifactBytes: 1024,
		GuestNamespace: "pilot-guest", ConductorNamespace: "pilot-conductor", ServerNamespace: "oberth"}
	id := strings.Repeat("d", 64)
	attempt := vmrunner.ConductorAttempt{AttemptID: "one", JobUID: "job-one", PodUID: "pod-one", PodName: "conductor-pod",
		NodeName: "worker-1", Container: "conductor", ContainerID: "containerd://" + id,
		ImageDigest: digest, SpecIdentity: strings.Repeat("f", 64), StartedAt: started}
	claim := vmrunner.ConductorAttachClaim{AttemptIdentity: vmrunner.ConductorAttemptIdentity(attempt),
		Challenge: strings.Repeat("5", 64), ClaimedAt: created.Add(90 * time.Second)}
	request := AttachRequest{Plan: plan, Attempt: attempt, Claim: claim, CreatedAt: created}
	if err := request.validate("worker-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCRI{status: &runtimev1.ContainerStatus{Id: id, Metadata: &runtimev1.ContainerMetadata{Name: "conductor"},
		State: runtimev1.ContainerState_CONTAINER_RUNNING, CreatedAt: started.Add(-time.Second).UnixNano(), StartedAt: started.UnixNano(),
		Image: &runtimev1.ImageSpec{Image: plan.ConductorImageRef}, ImageRef: digest, ImageId: digest},
		item: &runtimev1.Container{Id: id, PodSandboxId: "sandbox-one", Metadata: &runtimev1.ContainerMetadata{Name: "conductor"},
			State: runtimev1.ContainerState_CONTAINER_RUNNING, Image: &runtimev1.ImageSpec{Image: plan.ConductorImageRef}, ImageRef: digest, ImageId: digest},
		sandbox: &runtimev1.PodSandboxStatus{Id: "sandbox-one", State: runtimev1.PodSandboxState_SANDBOX_READY,
			Metadata: &runtimev1.PodSandboxMetadata{Uid: "pod-one", Name: "conductor-pod", Namespace: "pilot-conductor"}}}
	return request, fake, &Runtime{NodeName: "worker-1", CRI: fake}
}

func TestRuntimeAttachSelectsExactContainerSandboxAndStart(t *testing.T) {
	request, fake, runtime := runtimeFixture(t)
	url, observed, err := runtime.Attach(context.Background(), request)
	id := strings.Repeat("d", 64)
	if err != nil || url == "" || observed.RuntimeStartedAt.UnixNano() != fake.status.StartedAt || observed.SandboxID != "sandbox-one" || fake.attach != 1 ||
		fake.statusID != id || fake.listID != id || fake.sandboxID != "sandbox-one" || fake.attachID != id || fake.attachShape == nil ||
		!fake.attachShape.Stdin || !fake.attachShape.Stdout || !fake.attachShape.Stderr || fake.attachShape.Tty {
		t.Fatalf("exact runtime selection failed: %#v %q %d %v", observed, url, fake.attach, err)
	}
}

func TestRuntimeAttachRejectsConflictingCRIRecordsBeforeURL(t *testing.T) {
	for _, mode := range []string{"short-id", "status-id", "stopped", "image", "start", "list-id", "list-image", "sandbox-uid", "sandbox-namespace", "node", "replaced-after-attach"} {
		t.Run(mode, func(t *testing.T) {
			request, fake, runtime := runtimeFixture(t)
			switch mode {
			case "short-id":
				request.Attempt.ContainerID = "containerd://d"
				request.Claim.AttemptIdentity = vmrunner.ConductorAttemptIdentity(request.Attempt)
			case "status-id":
				fake.status.Id = strings.Repeat("a", 64)
			case "stopped":
				fake.status.State = runtimev1.ContainerState_CONTAINER_EXITED
			case "image":
				fake.status.ImageRef = "sha256:" + strings.Repeat("a", 64)
			case "start":
				fake.status.StartedAt += int64(time.Second)
			case "list-id":
				fake.item.Id = strings.Repeat("a", 64)
			case "list-image":
				fake.item.ImageRef = "sha256:" + strings.Repeat("a", 64)
			case "sandbox-uid":
				fake.sandbox.Metadata.Uid = "other-pod"
			case "sandbox-namespace":
				fake.sandbox.Metadata.Namespace = "other"
			case "node":
				request.Attempt.NodeName = "other-node"
				request.Claim.AttemptIdentity = vmrunner.ConductorAttemptIdentity(request.Attempt)
			case "replaced-after-attach":
				fake.change = func() { fake.status.StartedAt += int64(time.Second) }
			}
			if url, _, err := runtime.Attach(context.Background(), request); err == nil || url != "" {
				t.Fatalf("conflicting %s CRI evidence released URL: %q %v", mode, url, err)
			}
			if mode != "replaced-after-attach" && fake.attach != 0 {
				t.Fatalf("CRI attach called before rejecting %s", mode)
			}
		})
	}
}
