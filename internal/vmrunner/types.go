// Package vmrunner provides an operator-owned VM runner/controller for
// KubeVirt-based integration test execution.
//
// The runner manages VirtualMachineInstance (VMI) lifecycle on behalf of
// pipeline steps that require VM isolation. The pipeline declares what to
// test; the server decides how to run it. Evidence is collected server-side
// from the KubeVirt API, never from guest output.
//
// See docs/solutions/kubevirt-runner.md for the full design.
package vmrunner

import "time"

// VMRunSpec describes one VM-backed test execution. Every field is validated
// at admission before any VMI exists. The spec is immutable after admission:
// the server constructs the VMI from the admitted spec, not from a re-read.
type VMRunSpec struct {
	// RunID links this VM execution to its parent Oberth run.
	// Non-empty, validated at admission.
	RunID string

	// Repo is the repository under test.
	// Non-empty, validated at admission.
	Repo string

	// CandidateSHA is the exact source commit whose artifact is under test.
	// Must be a 40-character lowercase hex string (git SHA-1).
	CandidateSHA string

	// SuiteRevision is the test suite's pinned commit, resolved independently
	// of the candidate's checkout. This is the defence against a candidate
	// modifying the tests that evaluate it. 40-character lowercase hex.
	SuiteRevision string

	// GuestImageRef is the VM's boot image, which must be an OCI reference
	// pinned by digest (@sha256:...). A tag-only reference is rejected at
	// admission because it can drift between the decision and the boot.
	GuestImageRef string

	// Resources caps the VM's allocation.
	Resources VMResources

	// Deadline is the maximum wall time for the VM execution. Must be
	// positive and at most MaxDeadline.
	Deadline time.Duration
}

// VMResources bounds the VM's resource allocation. Every field has a minimum
// and a maximum, both enforced at admission.
type VMResources struct {
	// CPUCores is the number of virtual CPU cores. Range: [1, MaxVMCPUCores].
	CPUCores int

	// MemoryMiB is the VM's memory in mebibytes. Range: [MinVMMemoryMiB, MaxVMMemoryMiB].
	MemoryMiB int

	// DiskGiB is the VM's ephemeral disk in gibibytes. Range: [0, MaxVMDiskGiB].
	// Zero means no additional writable disk; the offline backend uses a RAM root.
	DiskGiB int
}

// VMInstance is the ownership receipt returned by Create. Its identity must be
// persisted by the scheduler before execution can contribute any evidence.
// A name alone is not authority to observe or delete a resource.
type VMInstance struct {
	Name         string
	UID          string
	SpecIdentity string
	CreatedAt    time.Time
}

// VMObservation is lifecycle state from the KubeVirt API, not a guest process
// result. Succeeded means voluntary shutdown; it carries no test exit status.
type VMObservation struct {
	Instance VMInstance
	Phase    string
}

// VMRunResult reports VM lifecycle and cleanup, not a passing test suite.
// The scheduler additionally requires an independently bound trusted receipt.
type VMRunResult struct {
	// Phase is the VMI's terminal phase: "Succeeded" or "Failed".
	Phase string

	// ExitCode is always ExitCodeUnknown: KubeVirt phase has no process exit code.
	ExitCode int32

	// CleanupComplete requires observed absence of this VMI and its launchers.
	CleanupComplete bool

	// Evidence is the server-collected evidence of the run.
	Evidence VMEvidence

	// Duration is how long the VM ran from creation to terminal phase.
	Duration time.Duration
}

// VMEvidence is the server-collected evidence of a VM execution. Every field
// is populated by the server, not by the guest. The CandidateSHA,
// SuiteRevision, and GuestImageRef are echoed from the admitted spec, not
// from anything the guest reports.
type VMEvidence struct {
	// CandidateSHA is echoed from the admitted spec.
	CandidateSHA string

	// SuiteRevision is echoed from the admitted spec.
	SuiteRevision string

	// GuestImageRef is echoed from the admitted spec.
	GuestImageRef string

	// VMInstanceName is the KubeVirt VMI's name in the pipeline namespace.
	VMInstanceName string

	// VMInstanceUID is the KubeVirt VMI's UID.
	VMInstanceUID string

	// StartedAt is when the VMI was created.
	StartedAt time.Time

	// FinishedAt is when the VMI reached a terminal phase.
	FinishedAt time.Time

	// ExitCodeSource is unavailable until a separate trusted suite receipt is
	// verified. Neither VMI phase nor a guest log marker supplies a process exit.
	ExitCodeSource string
}

const (
	ExitCodeUnknown       int32 = -1
	ExitCodeSourceUnknown       = "unavailable"
)
