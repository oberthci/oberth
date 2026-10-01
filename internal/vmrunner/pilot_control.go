package vmrunner

import (
	"errors"
	"net/netip"
)

// PilotEndpoint is public host-observed binding metadata, not fixture key
// material. The bootstrap adapter must bind the address to the actual launcher
// Pod IP and the leaf certificate to the ephemeral private key it delivered.
type PilotEndpoint struct {
	Attempt           string `json:"attempt"`
	VMIUID            string `json:"vmi_uid"`
	LauncherUID       string `json:"launcher_uid"`
	Address           string `json:"address"`
	ServerName        string `json:"server_name"`
	CertificateSHA256 string `json:"certificate_sha256"`
	FixtureGeneration string `json:"fixture_generation"`
}

func ValidatePilotEndpoint(endpoint PilotEndpoint) error {
	address, err := netip.ParseAddrPort(endpoint.Address)
	if err != nil || address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsMulticast() || address.Addr().Zone() != "" || address.String() != endpoint.Address {
		return errors.New("vmrunner: invalid pilot endpoint")
	}
	if (endpoint.Attempt != InitialVMResource && endpoint.Attempt != RestartVMResource) || !boundedPilotID(endpoint.VMIUID) || !boundedPilotID(endpoint.LauncherUID) || endpoint.ServerName != "beacon.fixture" || !hexDigest(endpoint.CertificateSHA256) || !hexDigest(endpoint.FixtureGeneration) {
		return errors.New("vmrunner: incomplete pilot endpoint binding")
	}
	return nil
}

// PilotRestartRequest is emitted on the bound conductor stream only at the
// restart case. Generic progress is never lifecycle authority. The host must
// durably claim this request once before changing any resource.
type PilotRestartRequest struct {
	Version   int           `json:"version"`
	Type      string        `json:"type"`
	Execution string        `json:"execution"`
	Attempt   string        `json:"attempt"`
	Inventory string        `json:"inventory"`
	Sequence  int           `json:"sequence"`
	Case      string        `json:"case"`
	Old       PilotEndpoint `json:"old"`
}

type PilotRestartResponse struct {
	Version   int           `json:"version"`
	Type      string        `json:"type"`
	Execution string        `json:"execution"`
	Attempt   string        `json:"attempt"`
	Operation string        `json:"operation"`
	OldVMIUID string        `json:"old_vmi_uid"`
	Endpoint  PilotEndpoint `json:"endpoint"`
}

type PilotRestart struct {
	Request  PilotRestartRequest
	Response *PilotRestartResponse
}

func PilotRestartIdentity(request PilotRestartRequest) string { return canonicalIdentity(request) }

func ValidatePilotRestartRequest(plan PilotPlan, attempt ConductorAttempt, request PilotRestartRequest) error {
	if request.Version != 1 || request.Type != "restart-beacon" || request.Execution != PilotIdentity(plan) || request.Attempt != ConductorAttemptIdentity(attempt) || request.Inventory != PilotInventoryIdentity(plan) || request.Sequence != 1 || request.Case != "restart-fresh-relay" || request.Old.Attempt != InitialVMResource {
		return errors.New("vmrunner: unbound restart operation")
	}
	return ValidatePilotEndpoint(request.Old)
}

func ValidatePilotRestartResponse(request PilotRestartRequest, response PilotRestartResponse) error {
	if response.Version != 1 || response.Type != "restart-complete" || response.Execution != request.Execution || response.Attempt != request.Attempt || response.Operation != PilotRestartIdentity(request) || response.OldVMIUID != request.Old.VMIUID {
		return errors.New("vmrunner: restart reply differs from its claimed operation")
	}
	fresh := response.Endpoint
	if err := ValidatePilotEndpoint(fresh); err != nil {
		return err
	}
	if fresh.Attempt != RestartVMResource || fresh.VMIUID == request.Old.VMIUID || fresh.LauncherUID == request.Old.LauncherUID || fresh.CertificateSHA256 == request.Old.CertificateSHA256 || fresh.FixtureGeneration != request.Old.FixtureGeneration || fresh.ServerName != request.Old.ServerName {
		return errors.New("vmrunner: restart reply lacks fresh exact resource and TLS bindings")
	}
	return nil
}
