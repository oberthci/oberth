package vmrunner

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	ConductorResource = "conductor"
	InitialVMResource = "beacon-0"
	RestartVMResource = "beacon-1"
	MaxPilotResources = 32
)

type PilotExecution struct {
	Plan      PilotPlan
	Submitted bool
	Cleaning  bool
	Cleaned   bool
	Failure   string
	Attempt   *ConductorAttempt
	Attach    *ConductorAttachClaim
	Receipt   *PilotReceipt
	Endpoint  *PilotEndpoint
	Restart   *PilotRestart
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ResourceIntent is a closed host-owned resource handle. Parent identifies a
// journalled Job or VMI; child ownership must be observed from the actual API.
type ResourceIntent struct {
	Key          string
	Kind         string
	Namespace    string
	Name         string
	SpecIdentity string
	Parent       string
}

type ResourceReceipt struct {
	UID          string
	SpecIdentity string
	OwnerUID     string
	CreatedAt    time.Time
	PodIP        string
}

type PilotResource struct {
	Intent    ResourceIntent
	Receipt   ResourceReceipt
	Submitted bool
	Rejected  bool
	Cleaning  bool
	Cleaned   bool
}

func ValidatePilotResource(plan PilotPlan, intent ResourceIntent) error {
	if err := ValidatePilotPlan(plan); err != nil {
		return err
	}
	if len(validation.IsDNS1123Label(intent.Key)) != 0 || len(validation.IsDNS1123Subdomain(intent.Name)) != 0 || !hexDigest(intent.SpecIdentity) {
		return errors.New("vmrunner: invalid pilot resource identity")
	}
	if intent.Namespace != plan.GuestNamespace && intent.Namespace != plan.ConductorNamespace {
		return errors.New("vmrunner: pilot resource is outside its admitted namespaces")
	}
	switch intent.Kind {
	case "Job":
		if intent.Key != ConductorResource || intent.Namespace != plan.ConductorNamespace || intent.Parent != "" {
			return errors.New("vmrunner: only the admitted conductor Job is supported")
		}
	case "VirtualMachineInstance":
		if (intent.Key != InitialVMResource && intent.Key != RestartVMResource) || intent.Namespace != plan.GuestNamespace || intent.Parent != "" {
			return errors.New("vmrunner: only two sequential Beacon VM attempts are supported")
		}
	case "Pod":
		if intent.Parent != ConductorResource && intent.Parent != InitialVMResource && intent.Parent != RestartVMResource {
			return errors.New("vmrunner: pilot Pod requires its admitted parent")
		}
		expectedNamespace := plan.GuestNamespace
		if intent.Parent == ConductorResource {
			expectedNamespace = plan.ConductorNamespace
		}
		if intent.Namespace != expectedNamespace {
			return errors.New("vmrunner: pilot Pod namespace differs from its parent")
		}
	case "Service":
		if intent.Key != "beacon-endpoint" || intent.Namespace != plan.GuestNamespace || intent.Parent != "" {
			return errors.New("vmrunner: only the admitted Beacon endpoint is supported")
		}
	case "NetworkPolicy":
		if intent.Parent != "" ||
			(intent.Key != "guest-isolation" || intent.Namespace != plan.GuestNamespace) &&
				(intent.Key != "conductor-isolation" || intent.Namespace != plan.ConductorNamespace) {
			return errors.New("vmrunner: only admitted pilot isolation resources are supported")
		}
	default:
		return errors.New("vmrunner: unsupported pilot resource kind")
	}
	return nil
}

// PilotJournal is called only by the operator runtime adapter. No method is an
// MCP/guest endpoint. A result reader alone cannot authorize an attempt or UID.
type PilotJournal interface {
	ReservePilot(context.Context, PilotPlan) (PilotExecution, error)
	PilotExecution(context.Context, string) (PilotExecution, error)
	SubmitPilot(context.Context, string) error
	AddPilotResource(context.Context, string, ResourceIntent) error
	SubmitPilotResource(context.Context, string, string) error
	BindPilotResource(context.Context, string, string, ResourceReceipt) error
	ObservePilotPod(context.Context, string, ResourceIntent, ResourceReceipt) error
	RejectPilotResource(context.Context, string, string) error
	BindConductorAttempt(context.Context, string, ConductorAttempt) error
	ClaimConductorAttach(context.Context, string, ConductorAttempt) (ConductorAttachClaim, bool, error)
	BindConductorAttach(context.Context, string, string, time.Time) error
	BindPilotEndpoint(context.Context, string, PilotEndpoint) error
	ClaimPilotRestart(context.Context, string, PilotRestartRequest) (bool, error)
	CompletePilotRestart(context.Context, string, PilotRestartResponse) error
	RecordPilotReceipt(context.Context, string, PilotReceipt) error
	FailPilot(context.Context, string, string) error
	BeginPilotCleanup(context.Context, string) error
	BeginPilotResourceCleanup(context.Context, string, string) error
	CompletePilotResourceCleanup(context.Context, string, string, ResourceReceipt) error
	CompletePilotCleanup(context.Context, string) error
	PilotResources(context.Context, string) ([]PilotResource, error)
	PendingPilots(context.Context) ([]PilotExecution, error)
	CompletedPilotReceipt(context.Context, string) (PilotReceipt, error)
}
