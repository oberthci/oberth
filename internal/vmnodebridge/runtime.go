// Package vmnodebridge contains the privileged, node-local CRI selector for
// the inactive Beacon pilot. Installation and runtime qualification are
// separate operator decisions; no scheduler provider constructs it today.
package vmnodebridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/oberthci/oberth/internal/vmrunner"
)

var errRuntimeIdentity = errors.New("vmnodebridge: runtime process identity differs from admitted attempt")

// AttachRequest contains no fixture capabilities, arbitrary URL or command.
// CreatedAt is the immutable journal admission time, not a client clock hint.
type AttachRequest struct {
	Plan      vmrunner.PilotPlan
	Attempt   vmrunner.ConductorAttempt
	Claim     vmrunner.ConductorAttachClaim
	CreatedAt time.Time
}

func (request AttachRequest) validate(node string, now time.Time) error {
	if vmrunner.ValidateConductorAttachClaim(request.Plan, request.Attempt, request.Claim) != nil ||
		!request.Claim.BoundAt.IsZero() || request.Attempt.NodeName != node ||
		request.CreatedAt.IsZero() || request.CreatedAt.After(request.Claim.ClaimedAt) ||
		!request.Claim.ClaimedAt.Before(request.CreatedAt.Add(request.Plan.Spec.Deadline)) ||
		!now.Before(request.CreatedAt.Add(request.Plan.Spec.Deadline)) ||
		request.Plan.Spec.RunID == "" {
		return errRuntimeIdentity
	}
	return nil
}

type Runtime struct {
	NodeName string
	CRI      runtimev1.RuntimeServiceClient
}

// DialRuntime opens only the operator-configured Unix CRI socket. The caller
// owns the connection and must not expose this broad client to Oberth or Pods.
func DialRuntime(socket, node string) (*Runtime, *grpc.ClientConn, error) {
	if !strings.HasPrefix(socket, "/") || strings.ContainsAny(socket, "\x00\r\n") || node == "" {
		return nil, nil, errRuntimeIdentity
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return &Runtime{NodeName: node, CRI: runtimev1.NewRuntimeServiceClient(conn)}, conn, nil
}

type Observation struct {
	RuntimeStartedAt time.Time
	SandboxID        string
}

// Inspect checks typed CRI links, not Pod labels or name-based kubelet attach.
// An exact full ID is mandatory because containerd also accepts prefixes.
func (runtime *Runtime) Inspect(ctx context.Context, request AttachRequest) (Observation, error) {
	var result Observation
	if runtime == nil || runtime.CRI == nil || request.validate(runtime.NodeName, time.Now()) != nil {
		return result, errRuntimeIdentity
	}
	id := strings.TrimPrefix(request.Attempt.ContainerID, "containerd://")
	statusReply, err := runtime.CRI.ContainerStatus(ctx, &runtimev1.ContainerStatusRequest{ContainerId: id})
	if err != nil || statusReply == nil {
		return result, errRuntimeIdentity
	}
	status := statusReply.GetStatus()
	if status == nil || status.GetId() != id || status.GetState() != runtimev1.ContainerState_CONTAINER_RUNNING ||
		status.GetMetadata() == nil || status.GetMetadata().GetName() != request.Attempt.Container ||
		status.GetImage() == nil || status.GetImage().GetImage() != request.Plan.ConductorImageRef ||
		status.GetImageRef() != request.Attempt.ImageDigest || status.GetCreatedAt() <= 0 ||
		status.GetStartedAt() < status.GetCreatedAt() || status.GetFinishedAt() != 0 {
		return result, errRuntimeIdentity
	}
	started := time.Unix(0, status.GetStartedAt()).UTC()
	if started.Before(request.CreatedAt) || !started.Before(request.CreatedAt.Add(request.Plan.Spec.Deadline)) || started.After(time.Now()) {
		return result, errRuntimeIdentity
	}
	observed := request.Attempt.StartedAt
	if observed.Nanosecond() == 0 {
		if !started.Truncate(time.Second).Equal(observed) {
			return result, errRuntimeIdentity
		}
	} else if !started.Equal(observed) {
		return result, errRuntimeIdentity
	}
	listReply, err := runtime.CRI.ListContainers(ctx, &runtimev1.ListContainersRequest{Filter: &runtimev1.ContainerFilter{Id: id}})
	if err != nil || listReply == nil || len(listReply.GetContainers()) != 1 {
		return result, errRuntimeIdentity
	}
	container := listReply.GetContainers()[0]
	if container == nil || container.GetId() != id || container.GetState() != runtimev1.ContainerState_CONTAINER_RUNNING ||
		container.GetMetadata() == nil || container.GetMetadata().GetName() != status.GetMetadata().GetName() ||
		container.GetImage() == nil || container.GetImage().GetImage() != status.GetImage().GetImage() ||
		container.GetImageRef() != status.GetImageRef() || container.GetImageId() != status.GetImageId() ||
		container.GetPodSandboxId() == "" {
		return result, errRuntimeIdentity
	}
	sandboxReply, err := runtime.CRI.PodSandboxStatus(ctx, &runtimev1.PodSandboxStatusRequest{PodSandboxId: container.GetPodSandboxId()})
	if err != nil || sandboxReply == nil {
		return result, errRuntimeIdentity
	}
	sandbox := sandboxReply.GetStatus()
	if sandbox == nil || sandbox.GetId() != container.GetPodSandboxId() || sandbox.GetState() != runtimev1.PodSandboxState_SANDBOX_READY ||
		sandbox.GetMetadata() == nil || sandbox.GetMetadata().GetUid() != request.Attempt.PodUID ||
		sandbox.GetMetadata().GetName() != request.Attempt.PodName || sandbox.GetMetadata().GetNamespace() != request.Plan.ConductorNamespace {
		return result, errRuntimeIdentity
	}
	return Observation{RuntimeStartedAt: started, SandboxID: sandbox.GetId()}, nil
}

// Attach asks CRI for one exact-ID stream only after inspection, then inspects
// again before consuming its transient URL. This narrows the race but is not
// a substitute for qualified no-ID/task-reuse through URL consumption.
func (runtime *Runtime) Attach(ctx context.Context, request AttachRequest) (string, Observation, error) {
	first, err := runtime.Inspect(ctx, request)
	if err != nil {
		return "", Observation{}, err
	}
	id := strings.TrimPrefix(request.Attempt.ContainerID, "containerd://")
	reply, err := runtime.CRI.Attach(ctx, &runtimev1.AttachRequest{ContainerId: id, Stdin: true, Stdout: true, Stderr: true, Tty: false})
	if err != nil || reply == nil || reply.GetUrl() == "" {
		return "", Observation{}, errRuntimeIdentity
	}
	second, err := runtime.Inspect(ctx, request)
	if err != nil || second != first {
		return "", Observation{}, errRuntimeIdentity
	}
	return reply.GetUrl(), second, nil
}
