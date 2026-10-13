package vmrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TrustedTestVerdict is the closed vocabulary of trusted test outcomes.
type TrustedTestVerdict string

const (
	VerdictPending TrustedTestVerdict = "pending"
	VerdictPassed  TrustedTestVerdict = "passed"
	VerdictRefused TrustedTestVerdict = "refused"
)

// TrustedTestReceipt is the durable evidence of one trusted test execution.
// Every field except Verdict/RefusalReason is immutable after creation.
// The verdict comes from the kubelet termination record of the server-appended
// trusted-tests leaf, never from repository-written output.
type TrustedTestReceipt struct {
	// RunID links this receipt to its parent Oberth run.
	RunID string `json:"run_id"`

	// Profile is the declared profile name from tests.yaml.
	Profile string `json:"profile"`

	// CandidateSHA is the exact source commit under test (40-char hex).
	CandidateSHA string `json:"candidate_sha"`

	// SuiteIdentity is the test suite's pinned revision (40-char hex),
	// resolved independently from the upstream default branch.
	SuiteIdentity string `json:"suite_identity"`

	// GuestDigest is the admitted guest image's digest (sha256:hex64).
	GuestDigest string `json:"guest_digest"`

	// ContractDigest is the SHA-256 of the serialized contract.
	ContractDigest string `json:"contract_digest"`

	// Verdict is the outcome: pending, passed, or refused.
	Verdict TrustedTestVerdict `json:"verdict"`

	// PodUID is the Kubernetes UID of the trusted-tests leaf's pod.
	PodUID string `json:"pod_uid,omitempty"`

	// ContainerID is the container runtime ID of the trusted leaf.
	ContainerID string `json:"container_id,omitempty"`

	// ExitCode is the kubelet-recorded exit code of the trusted leaf.
	ExitCode int32 `json:"exit_code"`

	// RestartCount is the kubelet-recorded restart count.
	RestartCount int32 `json:"restart_count"`

	// TerminationHash is the SHA-256 of the kubelet termination message.
	TerminationHash string `json:"termination_hash,omitempty"`

	// RefusalReason explains why the verdict is refused.
	RefusalReason string `json:"refusal_reason,omitempty"`

	// CreatedAt is when the receipt was first written.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the verdict was last updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// KubeletRecord is the server-read termination record from the trusted leaf's
// pod. Every field comes from the Kubernetes API, never from guest output.
type KubeletRecord struct {
	PodUID       string
	ContainerID  string
	ExitCode     int32
	RestartCount int32
	Message      string
	FinishedAt   time.Time
}

// TerminationMessageHash returns the SHA-256 hex of the termination message.
func TerminationMessageHash(message string) string {
	digest := sha256.Sum256([]byte(message))
	return hex.EncodeToString(digest[:])
}

// ReceiptDigest returns a deterministic SHA-256 of the receipt's identity fields.
func ReceiptDigest(receipt TrustedTestReceipt) string {
	body, _ := json.Marshal(struct {
		RunID          string `json:"run_id"`
		Profile        string `json:"profile"`
		CandidateSHA   string `json:"candidate_sha"`
		SuiteIdentity  string `json:"suite_identity"`
		GuestDigest    string `json:"guest_digest"`
		ContractDigest string `json:"contract_digest"`
		Verdict        string `json:"verdict"`
		PodUID         string `json:"pod_uid"`
		ContainerID    string `json:"container_id"`
		ExitCode       int32  `json:"exit_code"`
		RestartCount   int32  `json:"restart_count"`
	}{
		RunID:          receipt.RunID,
		Profile:        receipt.Profile,
		CandidateSHA:   receipt.CandidateSHA,
		SuiteIdentity:  receipt.SuiteIdentity,
		GuestDigest:    receipt.GuestDigest,
		ContractDigest: receipt.ContractDigest,
		Verdict:        string(receipt.Verdict),
		PodUID:         receipt.PodUID,
		ContainerID:    receipt.ContainerID,
		ExitCode:       receipt.ExitCode,
		RestartCount:   receipt.RestartCount,
	})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// EvaluateKubeletVerdict determines the verdict from a kubelet termination
// record. Every refusal is a typed reason. The negative receipt cases are:
//   - wrong_digest: the trusted leaf's image digest does not match the contract
//   - exit_nonzero: exit code != 0 (covers PASS-then-exit-1)
//   - restart_detected: restart count > 0
//   - timeout: the trusted leaf did not finish within the deadline
func EvaluateKubeletVerdict(record KubeletRecord, expectedDigest string, deadline time.Duration, startedAt time.Time) (TrustedTestVerdict, string) {
	if record.RestartCount > 0 {
		return VerdictRefused, fmt.Sprintf("restart_detected: restart count %d; the trusted leaf must run exactly once", record.RestartCount)
	}

	if record.ExitCode != 0 {
		return VerdictRefused, fmt.Sprintf("exit_nonzero: exit code %d; the trusted leaf must exit 0", record.ExitCode)
	}

	if !record.FinishedAt.IsZero() && !startedAt.IsZero() && record.FinishedAt.Sub(startedAt) > deadline {
		return VerdictRefused, fmt.Sprintf("timeout: execution took %s, deadline was %s", record.FinishedAt.Sub(startedAt).Truncate(time.Second), deadline)
	}

	return VerdictPassed, ""
}

// ValidateReceipt checks that a receipt has the required identity fields.
func ValidateReceipt(receipt TrustedTestReceipt) error {
	var problems []error
	if receipt.RunID == "" {
		problems = append(problems, errors.New("receipt: RunID is required"))
	}
	if receipt.Profile == "" {
		problems = append(problems, errors.New("receipt: Profile is required"))
	}
	if len(receipt.CandidateSHA) != 40 {
		problems = append(problems, fmt.Errorf("receipt: CandidateSHA must be 40 hex chars, got %q", receipt.CandidateSHA))
	}
	if len(receipt.SuiteIdentity) != 40 {
		problems = append(problems, fmt.Errorf("receipt: SuiteIdentity must be 40 hex chars, got %q", receipt.SuiteIdentity))
	}
	if receipt.GuestDigest == "" {
		problems = append(problems, errors.New("receipt: GuestDigest is required"))
	}
	if receipt.ContractDigest == "" {
		problems = append(problems, errors.New("receipt: ContractDigest is required"))
	}
	switch receipt.Verdict {
	case VerdictPending, VerdictPassed, VerdictRefused:
	default:
		problems = append(problems, fmt.Errorf("receipt: Verdict %q is not in the known vocabulary", receipt.Verdict))
	}
	return errors.Join(problems...)
}
