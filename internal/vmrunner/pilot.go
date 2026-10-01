package vmrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	BeaconTransportProfile = "beacon-transport-amd64-v2"
	BeaconArtifact         = "bin/beacon"
	MaxBeaconArtifactBytes = 64 << 20
)

// BeaconCases is the independently owned transport subset. It is neither the
// full historical P1/P3 suite nor authority for guest kernel/internal claims.
func BeaconCases() []string {
	return []string{
		"authenticated-relay", "wrong-tenant-key", "invalid-token", "replayed-token",
		"missing-v2-tenant", "near-match-tenant", "near-match-domain", "restart-fresh-relay",
	}
}

// PilotPlan is sealed after bounded artifact capture. Pre-build admission must
// authorize the independently selected recipe before these build outputs exist.
// It contains no fixture keys, URLs or candidate-selected commands.
type PilotPlan struct {
	Version            int
	Profile            string
	Spec               VMRunSpec
	ConductorImageRef  string
	KernelDigest       string
	InitramfsDigest    string
	GuestHelperDigest  string
	ArtifactDigest     string
	ArtifactBytes      int64
	GuestNamespace     string
	ConductorNamespace string
	ServerNamespace    string
}

func ValidatePilotPlan(plan PilotPlan) error {
	if body, err := json.Marshal(plan); err != nil || len(body) > 16<<10 {
		return errors.New("vmrunner: pilot plan exceeds its encoding bound")
	}
	if plan.Version != 1 || plan.Profile != BeaconTransportProfile {
		return errors.New("vmrunner: unsupported conductor profile")
	}
	if err := ValidateVMRunSpec(plan.Spec); err != nil {
		return err
	}
	if err := errors.Join(validateGuestImageRef(plan.ConductorImageRef)...); err != nil {
		return fmt.Errorf("vmrunner: conductor image: %w", err)
	}
	for _, digest := range []string{plan.KernelDigest, plan.InitramfsDigest, plan.GuestHelperDigest, plan.ArtifactDigest} {
		if !ociDigestPattern.MatchString(digest) {
			return errors.New("vmrunner: pilot inputs require exact SHA-256 digests")
		}
	}
	if plan.ArtifactBytes < 1 || plan.ArtifactBytes > MaxBeaconArtifactBytes {
		return errors.New("vmrunner: pilot artifact exceeds its byte bound")
	}
	if plan.Spec.Resources != (VMResources{CPUCores: 1, MemoryMiB: 512}) || plan.Spec.Deadline > 10*time.Minute {
		return errors.New("vmrunner: Beacon pilot requires one CPU, 512MiB RAM root and at most ten minutes")
	}
	for _, namespace := range []string{plan.GuestNamespace, plan.ConductorNamespace, plan.ServerNamespace} {
		if len(validation.IsDNS1123Label(namespace)) != 0 {
			return errors.New("vmrunner: pilot namespace is invalid")
		}
	}
	if plan.GuestNamespace == plan.ServerNamespace || plan.ConductorNamespace == plan.ServerNamespace || plan.GuestNamespace == plan.ConductorNamespace {
		return errors.New("vmrunner: guest, conductor and server namespaces must be distinct")
	}
	return nil
}

func PilotIdentity(plan PilotPlan) string { return canonicalIdentity(plan) }

func PilotInventoryIdentity(plan PilotPlan) string {
	return canonicalIdentity(struct {
		Profile, Suite string
		Cases          []string
	}{plan.Profile, plan.Spec.SuiteRevision, BeaconCases()})
}

func canonicalIdentity(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// ConductorAttempt is observed by the host runtime, not decoded from a guest or
// conductor result stream. One durable CAS binds this exact process attempt.
type ConductorAttempt struct {
	AttemptID    string
	JobUID       string
	PodUID       string
	PodName      string
	NodeName     string
	Container    string
	ContainerID  string
	ImageDigest  string
	SpecIdentity string
	StartedAt    time.Time
}

func ValidateConductorAttempt(plan PilotPlan, attempt ConductorAttempt) error {
	if err := ValidatePilotPlan(plan); err != nil {
		return err
	}
	for _, id := range []string{attempt.AttemptID, attempt.JobUID, attempt.PodUID, attempt.ContainerID} {
		if !boundedPilotID(id) {
			return errors.New("vmrunner: incomplete conductor process identity")
		}
	}
	if attempt.Container != "conductor" || !hexDigest(attempt.SpecIdentity) || attempt.StartedAt.IsZero() || attempt.StartedAt.After(time.Now()) || ConductorAttemptIdentity(attempt) == "" {
		return errors.New("vmrunner: invalid conductor process binding")
	}
	if (attempt.PodName == "") != (attempt.NodeName == "") ||
		(attempt.PodName != "" && (len(validation.IsDNS1123Subdomain(attempt.PodName)) != 0 || len(validation.IsDNS1123Subdomain(attempt.NodeName)) != 0)) {
		return errors.New("vmrunner: invalid conductor placement")
	}
	_, expectedDigest, ok := strings.Cut(plan.ConductorImageRef, "@")
	if !ok || attempt.ImageDigest != expectedDigest {
		return errors.New("vmrunner: observed conductor image differs from admission")
	}
	return nil
}

func boundedPilotID(value string) bool {
	return len(value) > 0 && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func ConductorAttemptIdentity(attempt ConductorAttempt) string { return canonicalIdentity(attempt) }

var fullContainerdID = regexp.MustCompile(`^containerd://[a-f0-9]{64}$`)

// ConductorAttachClaim is the spent, durable right to open one protected
// channel. A failed or ambiguous attach never makes this claim available again.
type ConductorAttachClaim struct {
	AttemptIdentity  string
	Challenge        string
	ClaimedAt        time.Time
	RuntimeStartedAt time.Time
	BoundAt          time.Time
}

func ValidateConductorAttachClaim(plan PilotPlan, attempt ConductorAttempt, claim ConductorAttachClaim) error {
	if err := ValidateConductorAttempt(plan, attempt); err != nil {
		return err
	}
	if attempt.PodName == "" || attempt.NodeName == "" || !fullContainerdID.MatchString(attempt.ContainerID) ||
		claim.AttemptIdentity != ConductorAttemptIdentity(attempt) || !hexDigest(claim.Challenge) ||
		claim.ClaimedAt.IsZero() || claim.ClaimedAt.After(time.Now()) ||
		(claim.BoundAt.IsZero() != claim.RuntimeStartedAt.IsZero()) {
		return errors.New("vmrunner: invalid conductor attach claim")
	}
	if !claim.BoundAt.IsZero() {
		if claim.BoundAt.Before(claim.ClaimedAt) || claim.BoundAt.After(time.Now()) || claim.RuntimeStartedAt.After(claim.BoundAt) {
			return errors.New("vmrunner: invalid conductor attach binding time")
		}
		if attempt.StartedAt.Nanosecond() == 0 {
			if !claim.RuntimeStartedAt.Truncate(time.Second).Equal(attempt.StartedAt) {
				return errors.New("vmrunner: runtime start differs from observed second")
			}
		} else if !claim.RuntimeStartedAt.Equal(attempt.StartedAt) {
			return errors.New("vmrunner: runtime start differs from observed process")
		}
	}
	return nil
}

func hexDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// ConductorTermination comes only from the exact host process/container record.
// A Job condition, guest shutdown or typed result event cannot supply this fact.
type ConductorTermination struct {
	Attempt      ConductorAttempt
	ExitCode     int32
	Signal       int32
	RestartCount int32
	Reason       string
	FinishedAt   time.Time
}

type PilotReceipt struct {
	Plan        PilotPlan
	Attempt     ConductorAttempt
	Termination ConductorTermination
	Results     ConductorResults
}

// VerifyPilotReceipt checks provisional execution evidence. Publication also
// requires the durable journal's observed cleanup of every owned resource.
func VerifyPilotReceipt(plan PilotPlan, receipt PilotReceipt) error {
	if err := ValidatePilotPlan(plan); err != nil {
		return err
	}
	if PilotIdentity(plan) != PilotIdentity(receipt.Plan) {
		return errors.New("vmrunner: receipt inputs differ from the sealed pilot plan")
	}
	if err := ValidateConductorAttempt(plan, receipt.Attempt); err != nil {
		return err
	}
	terminal := receipt.Termination
	if ConductorAttemptIdentity(receipt.Attempt) != ConductorAttemptIdentity(terminal.Attempt) ||
		terminal.ExitCode != 0 || terminal.Signal != 0 || terminal.RestartCount != 0 || terminal.Reason != "Completed" {
		return errors.New("vmrunner: conductor lacks one exact successful process termination")
	}
	if terminal.FinishedAt.Before(receipt.Attempt.StartedAt) || terminal.FinishedAt.After(receipt.Attempt.StartedAt.Add(plan.Spec.Deadline)) {
		return errors.New("vmrunner: conductor termination exceeds the admitted deadline")
	}
	return verifyConductorResults(plan, receipt.Attempt, receipt.Results)
}
