package vmrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validContract = "version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n"

const validV2Contract = `version: 2
profile: kvm-suite-amd64-v1
artifact: ebpf/secret_monitor.o
guest_image: europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/test-guest@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
resources:
  cpu_cores: 2
  memory_mib: 4096
timeout: 30m
`

func TestContractRefusesCandidateAuthority(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"empty": "", "oversized": strings.Repeat(" ", MaxContractBytes+1),
		"command":               validContract + "command: [sh, -c, true]\n",
		"identity":              validContract + "serviceAccountName: admin\n",
		"duplicate":             validContract + "profile: ebpf-offline-amd64-v1\n",
		"second document":       validContract + "--- # second\nversion: 1\n",
		"empty second document": validContract + "---\n",
		"anchor":                strings.ReplaceAll(validContract, "profile: ", "profile: &p "),
		"alias":                 "version: 1\nprofile: &p ebpf-offline-amd64-v1\nartifact: *p\n",
		"traversal":             strings.ReplaceAll(validContract, BPFArtifact, "../secret_monitor.o"),
		"other profile":         strings.ReplaceAll(validContract, OfflineEBPFProfile, "shell-v1"),
		"template":              strings.ReplaceAll(validContract, BPFArtifact, "'{{ inputs.parameters.path }}'"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeContract([]byte(body)); err == nil {
				t.Fatal("candidate authority or invalid contract was admitted")
			}
		})
	}
}

func TestContractV1RejectsV2Fields(t *testing.T) {
	t.Parallel()
	// A version 1 contract must not carry v2 fields — KnownFields rejects them.
	for name, body := range map[string]string{
		"guest_image":                         validContract + "guest_image: some-image@sha256:abc\n",
		"resources":                           validContract + "resources:\n  cpu_cores: 2\n  memory_mib: 4096\n",
		"timeout":                             validContract + "timeout: 30m\n",
		"unknown_key":                         validContract + "custom_field: value\n",
		"wrong version as v2 upgrade attempt": strings.ReplaceAll(validContract, "version: 1", "version: 2"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeContract([]byte(body)); err == nil {
				t.Fatalf("v1 contract with v2 field %q was admitted", name)
			}
		})
	}
}

func TestContractV2AcceptsValidProfile(t *testing.T) {
	t.Parallel()
	contract, err := DecodeContract([]byte(validV2Contract))
	if err != nil {
		t.Fatal(err)
	}
	if contract.Version != 2 {
		t.Fatalf("version = %d, want 2", contract.Version)
	}
	if contract.Profile != KVMSuiteProfile {
		t.Fatalf("profile = %q, want %q", contract.Profile, KVMSuiteProfile)
	}
	if contract.Artifact != BPFArtifact {
		t.Fatalf("artifact = %q, want %q", contract.Artifact, BPFArtifact)
	}
	if !strings.Contains(contract.GuestImage, "@sha256:") {
		t.Fatalf("guest_image = %q, want digest-pinned", contract.GuestImage)
	}
	if contract.Resources.CPUCores != 2 || contract.Resources.MemoryMiB != 4096 {
		t.Fatalf("resources = %+v, want cpu_cores=2 memory_mib=4096", contract.Resources)
	}
	if contract.Timeout != 30*time.Minute {
		t.Fatalf("timeout = %s, want 30m0s", contract.Timeout)
	}
}

func TestContractV2RejectsInvalid(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"unknown profile": strings.ReplaceAll(validV2Contract, KVMSuiteProfile, "unknown-profile-v1"),
		"empty artifact":  strings.ReplaceAll(validV2Contract, "artifact: ebpf/secret_monitor.o", "artifact: \"\""),
		"tag-only image": strings.ReplaceAll(validV2Contract,
			"europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/test-guest@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			"test-image:latest"),
		"missing image":     strings.ReplaceAll(validV2Contract, "guest_image: europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/test-guest@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", "guest_image: \"\""),
		"cpu too high":      strings.ReplaceAll(validV2Contract, "cpu_cores: 2", "cpu_cores: 99"),
		"cpu zero":          strings.ReplaceAll(validV2Contract, "cpu_cores: 2", "cpu_cores: 0"),
		"memory too low":    strings.ReplaceAll(validV2Contract, "memory_mib: 4096", "memory_mib: 64"),
		"memory too high":   strings.ReplaceAll(validV2Contract, "memory_mib: 4096", "memory_mib: 99999"),
		"timeout too short": strings.ReplaceAll(validV2Contract, "timeout: 30m", "timeout: 10s"),
		"timeout too long":  strings.ReplaceAll(validV2Contract, "timeout: 30m", "timeout: 24h"),
		"invalid timeout":   strings.ReplaceAll(validV2Contract, "timeout: 30m", "timeout: not-a-duration"),
		"unknown v2 key":    validV2Contract + "custom_field: injected\n",
		"command injection": validV2Contract + "command: echo pass\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeContract([]byte(body)); err == nil {
				t.Fatalf("invalid v2 contract %q was admitted", name)
			}
		})
	}
}

func TestContractV2RejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	body := strings.ReplaceAll(validV2Contract, "version: 2", "version: 99")
	_, err := DecodeContract([]byte(body))
	if err == nil {
		t.Fatal("version 99 was admitted")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %q, want mention of unsupported version", err.Error())
	}
}

func TestContractV2RequiredContractMismatch(t *testing.T) {
	t.Parallel()
	v2a, err := DecodeContract([]byte(validV2Contract))
	if err != nil {
		t.Fatal(err)
	}
	v2b := v2a
	v2b.Timeout = 45 * time.Minute
	if _, err := RequiredContract(&v2a, &v2b); err == nil {
		t.Fatal("mismatched v2 contracts admitted")
	}
}

func TestContractV2DigestDiffersFromV1(t *testing.T) {
	t.Parallel()
	v1, err := DecodeContract([]byte(validContract))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := DecodeContract([]byte(validV2Contract))
	if err != nil {
		t.Fatal(err)
	}
	if ContractDigest(&v1) == ContractDigest(&v2) {
		t.Fatal("v1 and v2 digests collide")
	}
}

func TestRequiredContractRetainsDeletedBaseline(t *testing.T) {
	t.Parallel()
	baseline, err := DecodeContract([]byte(validContract))
	if err != nil {
		t.Fatal(err)
	}
	required, err := RequiredContract(nil, &baseline)
	if err != nil || required == nil || !contractEqual(*required, baseline) {
		t.Fatalf("required = %v, err = %v", required, err)
	}
	if ContractDigest(required) == ContractDigest(nil) {
		t.Fatal("required suite aliases absent policy")
	}
	required.Profile = "modified"
	if baseline.Profile != OfflineEBPFProfile {
		t.Fatal("returned contract mutates policy")
	}
}

func TestReadContractRefusesSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "tests.yaml")
	if err := os.WriteFile(outside, []byte(validContract), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ContractFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadContract(root); err == nil {
		t.Fatal("symlink contract admitted")
	}
}

func TestReadContractDistinguishesMissingAndInvalid(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if c, err := ReadContract(root); err != nil || c != nil {
		t.Fatalf("missing contract = %v, %v", c, err)
	}
	if err := os.Mkdir(filepath.Join(root, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ContractFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadContract(root); err == nil {
		t.Fatal("empty file treated as absent")
	}
	if err := os.WriteFile(filepath.Join(root, ContractFile), []byte(validContract), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := ReadContract(root); err != nil || c == nil || c.Profile != OfflineEBPFProfile {
		t.Fatalf("valid contract = %v, %v", c, err)
	}
}

func FuzzDecodeContract(f *testing.F) {
	f.Add([]byte(validContract))
	f.Add([]byte(validV2Contract))
	f.Fuzz(func(t *testing.T, source []byte) {
		contract, err := DecodeContract(source)
		if err != nil {
			return
		}
		switch contract.Version {
		case 1:
			if contract.Profile != OfflineEBPFProfile || contract.Artifact != BPFArtifact {
				t.Fatal("admitted v1 contract escaped fixed vocabulary")
			}
		case 2:
			if !knownV2Profiles[contract.Profile] {
				t.Fatal("admitted v2 contract escaped profile vocabulary")
			}
		default:
			t.Fatalf("admitted contract with unknown version %d", contract.Version)
		}
	})
}
