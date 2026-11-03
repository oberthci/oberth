package vmrunner

import (
	"strings"
	"testing"
	"time"
)

// --- Admission validation tests ---

func validSpec() VMRunSpec {
	return VMRunSpec{
		RunID:         "run-abc123",
		Repo:          "acme-ebpf",
		CandidateSHA:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		SuiteRevision: "f6e5d4c3b2a1f6e5d4c3b2a1f6e5d4c3b2a1f6e5",
		GuestImageRef: "registry.example.com/guest:v1@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Resources: VMResources{
			CPUCores:  2,
			MemoryMiB: 4096,
			DiskGiB:   10,
		},
		Deadline: 30 * time.Minute,
	}
}

func TestValidateVMRunSpec_ValidSpec(t *testing.T) {
	if err := ValidateVMRunSpec(validSpec()); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestValidateVMRunSpec_EmptyRunID(t *testing.T) {
	spec := validSpec()
	spec.RunID = ""
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "RunID is required") {
		t.Fatalf("expected RunID rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_EmptyRepo(t *testing.T) {
	spec := validSpec()
	spec.Repo = ""
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "Repo is required") {
		t.Fatalf("expected Repo rejection, got: %v", err)
	}
}

// --- Candidate SHA validation ---

func TestValidateVMRunSpec_EmptyCandidateSHA(t *testing.T) {
	spec := validSpec()
	spec.CandidateSHA = ""
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CandidateSHA") {
		t.Fatalf("expected CandidateSHA rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ShortCandidateSHA(t *testing.T) {
	spec := validSpec()
	spec.CandidateSHA = "abc123"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CandidateSHA") {
		t.Fatalf("expected CandidateSHA rejection for short SHA, got: %v", err)
	}
}

func TestValidateVMRunSpec_UppercaseCandidateSHA(t *testing.T) {
	spec := validSpec()
	spec.CandidateSHA = "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CandidateSHA") {
		t.Fatalf("expected CandidateSHA rejection for uppercase, got: %v", err)
	}
}

func TestValidateVMRunSpec_InjectionInCandidateSHA(t *testing.T) {
	spec := validSpec()
	spec.CandidateSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3'; DROP"
	err := ValidateVMRunSpec(spec)
	if err == nil {
		t.Fatal("expected CandidateSHA rejection for injection attempt")
	}
}

// --- Suite revision validation ---

func TestValidateVMRunSpec_EmptySuiteRevision(t *testing.T) {
	spec := validSpec()
	spec.SuiteRevision = ""
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "SuiteRevision") {
		t.Fatalf("expected SuiteRevision rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_InvalidSuiteRevision(t *testing.T) {
	spec := validSpec()
	spec.SuiteRevision = "not-a-sha"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "SuiteRevision") {
		t.Fatalf("expected SuiteRevision rejection, got: %v", err)
	}
}

// --- Adversarial: candidate modifies suite revision (I1 violation) ---

func TestValidateVMRunSpec_CandidateCannotModifySuiteRevision(t *testing.T) {
	// A spec where CandidateSHA == SuiteRevision means the candidate tests
	// itself with its own test suite. This defeats invariant I1.
	spec := validSpec()
	spec.SuiteRevision = spec.CandidateSHA // Same commit for both
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CandidateSHA and SuiteRevision must differ") {
		t.Fatalf("expected rejection when candidate equals suite, got: %v", err)
	}
}

// --- Guest image validation ---

func TestValidateVMRunSpec_EmptyGuestImageRef(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = ""
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "GuestImageRef is required") {
		t.Fatalf("expected GuestImageRef rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_TagOnlyGuestImageRef(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "registry.example.com/guest:latest"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "pinned by digest") {
		t.Fatalf("expected tag-only rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_InvalidDigest(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "registry.example.com/guest@sha256:tooshort"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "valid sha256 digest") {
		t.Fatalf("expected invalid digest rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_EmptyRepository(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "no repository before the digest") {
		t.Fatalf("expected empty repository rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_MD5Digest(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "registry.example.com/guest@md5:abcdef0123456789"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "valid sha256 digest") {
		t.Fatalf("expected md5 rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ControlCharsInGuestImageRef(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "registry.example.com/guest\x00evil@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Fatalf("expected rejection of control characters in GuestImageRef, got: %v", err)
	}
}

func TestValidateVMRunSpec_SpaceInGuestImageRef(t *testing.T) {
	spec := validSpec()
	spec.GuestImageRef = "registry.example.com/guest image@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Fatalf("expected rejection of space in GuestImageRef, got: %v", err)
	}
}

// --- Resource bounds ---

func TestValidateVMRunSpec_ZeroCPU(t *testing.T) {
	spec := validSpec()
	spec.Resources.CPUCores = 0
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CPUCores must be at least 1") {
		t.Fatalf("expected zero CPU rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ExcessiveCPU(t *testing.T) {
	spec := validSpec()
	spec.Resources.CPUCores = MaxVMCPUCores + 1
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "CPUCores") {
		t.Fatalf("expected excessive CPU rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_LowMemory(t *testing.T) {
	spec := validSpec()
	spec.Resources.MemoryMiB = 128
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "MemoryMiB must be at least") {
		t.Fatalf("expected low memory rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ExcessiveMemory(t *testing.T) {
	spec := validSpec()
	spec.Resources.MemoryMiB = MaxVMMemoryMiB + 1
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "MemoryMiB") {
		t.Fatalf("expected excessive memory rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_NegativeDisk(t *testing.T) {
	spec := validSpec()
	spec.Resources.DiskGiB = -1
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "DiskGiB must be non-negative") {
		t.Fatalf("expected negative disk rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ExcessiveDisk(t *testing.T) {
	spec := validSpec()
	spec.Resources.DiskGiB = MaxVMDiskGiB + 1
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "DiskGiB") {
		t.Fatalf("expected excessive disk rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ZeroDisk(t *testing.T) {
	spec := validSpec()
	spec.Resources.DiskGiB = 0
	// Zero disk is valid: the offline guest uses a RAM root with no extra disk.
	if err := ValidateVMRunSpec(spec); err != nil {
		t.Fatalf("zero disk should be valid, got: %v", err)
	}
}

// --- Deadline bounds ---

func TestValidateVMRunSpec_ZeroDeadline(t *testing.T) {
	spec := validSpec()
	spec.Deadline = 0
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "Deadline") {
		t.Fatalf("expected zero deadline rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_TooShortDeadline(t *testing.T) {
	spec := validSpec()
	spec.Deadline = 30 * time.Second
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "below the") {
		t.Fatalf("expected short deadline rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_ExcessiveDeadline(t *testing.T) {
	spec := validSpec()
	spec.Deadline = MaxDeadline + time.Hour
	err := ValidateVMRunSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("expected excessive deadline rejection, got: %v", err)
	}
}

func TestValidateVMRunSpec_MaxDeadlineExact(t *testing.T) {
	spec := validSpec()
	spec.Deadline = MaxDeadline
	if err := ValidateVMRunSpec(spec); err != nil {
		t.Fatalf("max deadline should be valid, got: %v", err)
	}
}

func TestValidateVMRunSpec_MinDeadlineExact(t *testing.T) {
	spec := validSpec()
	spec.Deadline = MinDeadline
	if err := ValidateVMRunSpec(spec); err != nil {
		t.Fatalf("min deadline should be valid, got: %v", err)
	}
}

// --- Multiple validation errors ---

func TestValidateVMRunSpec_MultipleErrors(t *testing.T) {
	spec := VMRunSpec{} // Every field is invalid.
	err := ValidateVMRunSpec(spec)
	if err == nil {
		t.Fatal("expected multiple errors for empty spec")
	}
	msg := err.Error()
	expected := []string{"RunID", "Repo", "CandidateSHA", "SuiteRevision", "GuestImageRef", "CPUCores", "MemoryMiB", "Deadline"}
	for _, keyword := range expected {
		if !strings.Contains(msg, keyword) {
			t.Errorf("expected error to mention %q, got: %s", keyword, msg)
		}
	}
}

// --- Spec identity ---

func TestSpecIdentity_Deterministic(t *testing.T) {
	spec := validSpec()
	id1 := SpecIdentity(spec)
	id2 := SpecIdentity(spec)
	if id1 != id2 {
		t.Fatalf("SpecIdentity is not deterministic: %q != %q", id1, id2)
	}
}

func TestSpecIdentity_DiffersOnCandidateChange(t *testing.T) {
	spec := validSpec()
	id1 := SpecIdentity(spec)
	spec.CandidateSHA = "b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3"
	id2 := SpecIdentity(spec)
	if id1 == id2 {
		t.Fatal("SpecIdentity should differ when CandidateSHA changes")
	}
}

func TestSpecIdentity_DiffersOnSuiteChange(t *testing.T) {
	spec := validSpec()
	id1 := SpecIdentity(spec)
	spec.SuiteRevision = "0000000000000000000000000000000000000000"
	id2 := SpecIdentity(spec)
	if id1 == id2 {
		t.Fatal("SpecIdentity should differ when SuiteRevision changes")
	}
}

func TestSpecIdentity_DiffersOnImageChange(t *testing.T) {
	spec := validSpec()
	id1 := SpecIdentity(spec)
	spec.GuestImageRef = "other.registry.com/guest@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	id2 := SpecIdentity(spec)
	if id1 == id2 {
		t.Fatal("SpecIdentity should differ when GuestImageRef changes")
	}
}

func TestSpecIdentity_DiffersOnResourceChange(t *testing.T) {
	spec := validSpec()
	id1 := SpecIdentity(spec)
	spec.Resources.CPUCores = 4
	id2 := SpecIdentity(spec)
	if id1 == id2 {
		t.Fatal("SpecIdentity should differ when Resources change")
	}
}

func TestSpecIdentity_NulCollisionResistance(t *testing.T) {
	// Under the old \x00-joined scheme, these two specs produce identical
	// identity strings because an embedded NUL shifts a field boundary:
	//   A: "run\x00extra" + \x00 + "repo"  = "run\x00extra\x00repo"
	//   B: "run"          + \x00 + "extra\x00repo" = "run\x00extra\x00repo"
	// Length-prefixed hashing makes them distinct because the per-field
	// lengths differ ([3]"run" + [5]"extra" vs [3]"run" + [10]"extra\x00repo").
	specA := validSpec()
	specA.RunID = "run\x00extra"
	specA.Repo = "repo"

	specB := validSpec()
	specB.RunID = "run"
	specB.Repo = "extra\x00repo"

	idA := SpecIdentity(specA)
	idB := SpecIdentity(specB)
	if idA == idB {
		t.Fatalf("SpecIdentity must produce distinct digests for specs whose "+
			"fields differ only by embedded NUL boundaries; both produced %s", idA)
	}
}

func TestSpecIdentity_IsHexSHA256(t *testing.T) {
	spec := validSpec()
	id := SpecIdentity(spec)
	if len(id) != 64 {
		t.Fatalf("SpecIdentity should return a 64-char hex SHA-256 digest, got %d chars: %s", len(id), id)
	}
	for _, c := range id {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Fatalf("SpecIdentity contains non-hex character %q in %s", c, id)
		}
	}
}
