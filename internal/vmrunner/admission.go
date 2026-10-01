package vmrunner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Admission ceilings. A repository-authored VM run spec is untrusted input;
// every unbounded dimension gets a ceiling before any VMI exists.
const (
	MaxVMCPUCores  = 4
	MinVMMemoryMiB = 512
	MaxVMMemoryMiB = 16384
	MaxVMDiskGiB   = 64
	MinDeadline    = 1 * time.Minute
	MaxDeadline    = 6 * time.Hour
)

// hexSHA1Pattern matches a 40-character lowercase hexadecimal string (git SHA-1).
var hexSHA1Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ociDigestPattern matches an OCI content-addressable digest (sha256:hex64).
var ociDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ociRefRepoPattern matches valid characters in the repository portion of an
// OCI image reference (before the @digest). Rejects control characters, NUL
// bytes, spaces, and metacharacters that could cause identity collisions or
// injection in downstream consumers (issue #410).
var ociRefRepoPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)

// ValidateVMRunSpec validates every field of a VM run specification before
// any VMI is created. Every rejection names the exact field and the reason.
//
// AI-INVARIANT: this runs before any Kubernetes object is created. A field
// that passes validation here is the exact value that will appear in the VMI
// and in the evidence. There is no re-read path between admission and VMI
// construction.
func ValidateVMRunSpec(spec VMRunSpec) error {
	var problems []error

	// Identity fields -- link the VM execution to its parent run.
	if strings.TrimSpace(spec.RunID) == "" {
		problems = append(problems, errors.New("vmrunner: RunID is required"))
	}
	if strings.TrimSpace(spec.Repo) == "" {
		problems = append(problems, errors.New("vmrunner: Repo is required"))
	}

	// Candidate binding -- the exact source commit under test.
	if !hexSHA1Pattern.MatchString(spec.CandidateSHA) {
		problems = append(problems, fmt.Errorf(
			"vmrunner: CandidateSHA must be a 40-character lowercase hex string, got %q", spec.CandidateSHA))
	}

	// Suite revision pinning -- resolved independently of the candidate.
	// This is invariant I1: the candidate cannot modify the test suite.
	if !hexSHA1Pattern.MatchString(spec.SuiteRevision) {
		problems = append(problems, fmt.Errorf(
			"vmrunner: SuiteRevision must be a 40-character lowercase hex string, got %q", spec.SuiteRevision))
	}

	// Guest image pinning -- must be pinned by digest, never by tag alone.
	problems = append(problems, validateGuestImageRef(spec.GuestImageRef)...)

	// Resource bounds.
	problems = append(problems, validateResources(spec.Resources)...)

	// Deadline bounds.
	if spec.Deadline < MinDeadline {
		problems = append(problems, fmt.Errorf(
			"vmrunner: Deadline %s is below the %s minimum", spec.Deadline, MinDeadline))
	}
	if spec.Deadline > MaxDeadline {
		problems = append(problems, fmt.Errorf(
			"vmrunner: Deadline %s exceeds the %s ceiling", spec.Deadline, MaxDeadline))
	}

	// Cross-field: candidate and suite must differ. A spec where the candidate
	// IS the suite revision means the candidate tests itself with its own
	// tests, which defeats invariant I1.
	if spec.CandidateSHA != "" && spec.CandidateSHA == spec.SuiteRevision {
		problems = append(problems, errors.New(
			"vmrunner: CandidateSHA and SuiteRevision must differ; a candidate cannot test itself with its own test suite"))
	}

	return errors.Join(problems...)
}

// validateGuestImageRef validates that the reference is a valid OCI image
// reference pinned by digest. Tag-only references are rejected because they
// can drift between the admission decision and the VM boot.
func validateGuestImageRef(ref string) []error {
	var problems []error
	if strings.TrimSpace(ref) == "" {
		problems = append(problems, errors.New("vmrunner: GuestImageRef is required"))
		return problems
	}

	// Must contain @sha256: digest
	atIndex := strings.LastIndex(ref, "@")
	if atIndex < 0 {
		problems = append(problems, fmt.Errorf(
			"vmrunner: GuestImageRef %q must be pinned by digest (@sha256:...); tag-only references drift", ref))
		return problems
	}

	// Repository part before @ must be non-empty and contain only valid
	// OCI reference characters. Control characters (including NUL) are
	// rejected to prevent SpecIdentity collision via embedded delimiters.
	repository := ref[:atIndex]
	if strings.TrimSpace(repository) == "" {
		problems = append(problems, fmt.Errorf(
			"vmrunner: GuestImageRef %q has no repository before the digest", ref))
		return problems
	}
	if !ociRefRepoPattern.MatchString(repository) {
		problems = append(problems, fmt.Errorf(
			"vmrunner: GuestImageRef repository %q contains invalid characters; only [a-zA-Z0-9._:/-] are permitted", repository))
		return problems
	}

	// Digest part after @ must be a valid sha256 digest
	digest := ref[atIndex+1:]
	if !ociDigestPattern.MatchString(digest) {
		problems = append(problems, fmt.Errorf(
			"vmrunner: GuestImageRef digest %q is not a valid sha256 digest (expected sha256:<64 hex chars>)", digest))
	}

	return problems
}

// validateResources enforces resource bounds.
func validateResources(resources VMResources) []error {
	var problems []error

	if resources.CPUCores < 1 {
		problems = append(problems, fmt.Errorf(
			"vmrunner: CPUCores must be at least 1, got %d", resources.CPUCores))
	}
	if resources.CPUCores > MaxVMCPUCores {
		problems = append(problems, fmt.Errorf(
			"vmrunner: CPUCores %d exceeds the %d ceiling", resources.CPUCores, MaxVMCPUCores))
	}

	if resources.MemoryMiB < MinVMMemoryMiB {
		problems = append(problems, fmt.Errorf(
			"vmrunner: MemoryMiB must be at least %d, got %d", MinVMMemoryMiB, resources.MemoryMiB))
	}
	if resources.MemoryMiB > MaxVMMemoryMiB {
		problems = append(problems, fmt.Errorf(
			"vmrunner: MemoryMiB %d exceeds the %d ceiling", resources.MemoryMiB, MaxVMMemoryMiB))
	}

	if resources.DiskGiB < 0 {
		problems = append(problems, fmt.Errorf(
			"vmrunner: DiskGiB must be non-negative, got %d", resources.DiskGiB))
	}
	if resources.DiskGiB > MaxVMDiskGiB {
		problems = append(problems, fmt.Errorf(
			"vmrunner: DiskGiB %d exceeds the %d ceiling", resources.DiskGiB, MaxVMDiskGiB))
	}

	return problems
}

// SpecIdentity returns a deterministic hex-encoded SHA-256 digest of the VM
// run specification. Two specs with the same identity produce the same VMI.
// This is used to detect whether a retried submission matches the original.
//
// Each field is length-prefixed (4-byte big-endian length + value) before
// hashing, so embedded delimiters in any field cannot shift a boundary and
// cause two distinct specs to collide (issue #410).
func SpecIdentity(spec VMRunSpec) string {
	h := sha256.New()
	writeField := func(s string) {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], uint32(len(s))) //nolint:gosec // spec fields are validated; none exceed uint32
		h.Write(buf[:])
		h.Write([]byte(s))
	}
	writeField(spec.RunID)
	writeField(spec.Repo)
	writeField(spec.CandidateSHA)
	writeField(spec.SuiteRevision)
	writeField(spec.GuestImageRef)
	writeField(fmt.Sprintf("%d", spec.Resources.CPUCores))
	writeField(fmt.Sprintf("%d", spec.Resources.MemoryMiB))
	writeField(fmt.Sprintf("%d", spec.Resources.DiskGiB))
	writeField(spec.Deadline.String())
	return hex.EncodeToString(h.Sum(nil))
}
