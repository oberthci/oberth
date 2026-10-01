package vmrunner

import (
	"os"
	"path/filepath"
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

func TestRequiredContractRetainsDeletedBaseline(t *testing.T) {
	t.Parallel()
	baseline, err := DecodeContract([]byte(validContract))
	if err != nil {
		t.Fatal(err)
	}
	required, err := RequiredContract(nil, &baseline)
	if err != nil || required == nil || *required != baseline {
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
		if err == nil && (contract.Version != 1 || contract.Profile != OfflineEBPFProfile || contract.Artifact != BPFArtifact) {
			t.Fatal("admitted contract escaped fixed vocabulary")
		}
	})
}
