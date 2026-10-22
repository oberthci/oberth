package vmrunner

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validContract = "version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n"

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
		"wrong version":         strings.ReplaceAll(validContract, "version: 1", "version: 2"),
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

func TestRequiredContractCopiesUnchangedBaseline(t *testing.T) {
	t.Parallel()
	baseline, err := DecodeContract([]byte(validContract))
	if err != nil {
		t.Fatal(err)
	}
	required, err := RequiredContract(&baseline, &baseline, nil)
	if err != nil || required == nil || !reflect.DeepEqual(*required, baseline) {
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
	f.Fuzz(func(t *testing.T, source []byte) {
		contract, err := DecodeContract(source)
		if err == nil && ValidateContract(contract) != nil {
			t.Fatal("admitted contract escaped fixed vocabulary")
		}
	})
}

func TestRequiredContractAllowsOnlyApprovedPairedRotation(t *testing.T) {
	baseline := kvmContractFixture()
	rotated := baseline
	rotated.SuiteSHA256, rotated.GuestSHA256 = strings.Repeat("c", 64), strings.Repeat("d", 64)
	for _, mode := range []string{"approved", "unapproved", "no authority", "suite only", "guest only", "removed", "profile", "version", "artifacts", "artifact order"} {
		t.Run(mode, func(t *testing.T) {
			original, candidate := baseline, rotated
			var selected = &candidate
			calls := 0
			approve := func(c Contract) error {
				calls++
				if mode == "unapproved" || c.SuiteSHA256 != rotated.SuiteSHA256 || c.GuestSHA256 != rotated.GuestSHA256 {
					return errors.New("pair is not operator-approved")
				}
				return nil
			}
			switch mode {
			case "no authority":
				approve = nil
			case "suite only":
				candidate.GuestSHA256 = baseline.GuestSHA256
			case "guest only":
				candidate.SuiteSHA256 = baseline.SuiteSHA256
			case "removed":
				selected = nil
			case "profile":
				candidate.Profile = OfflineEBPFProfile
			case "version":
				candidate = Contract{Version: 1, Profile: OfflineEBPFProfile, Artifact: BPFArtifact}
			case "artifacts":
				candidate.Artifacts = []string{"different.o"}
			case "artifact order":
				original.Artifacts = []string{"one.o", "two.o"}
				candidate.Artifacts = []string{"two.o", "one.o"}
			}
			got, err := RequiredContract(selected, &original, approve)
			if mode == "approved" {
				if err != nil || !reflect.DeepEqual(got, &candidate) || calls != 1 {
					t.Fatalf("approved rotation = %#v, %v; approval calls=%d", got, err, calls)
				}
				got.Artifacts[0] = "changed"
				if candidate.Artifacts[0] != BPFArtifact {
					t.Fatal("rotation aliases candidate artifacts")
				}
			} else if err == nil || got != nil {
				t.Fatalf("unsafe rotation admitted: %#v, %v", got, err)
			}
			if mode != "approved" && mode != "unapproved" && calls != 0 {
				t.Fatal("invalid contract reached recipe approval")
			}
		})
	}
}
