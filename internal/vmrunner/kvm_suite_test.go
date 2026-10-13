package vmrunner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func kvmContractFixture() Contract {
	return Contract{Version: 2, Profile: KVMSuiteAMD64Profile, SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), Artifacts: []string{"ebpf/secret_monitor.o"}}
}

func kvmContractYAML() string {
	c := kvmContractFixture()
	return fmt.Sprintf("version: 2\nprofile: %s\nsuite_sha256: %s\nguest_sha256: %s\nartifacts: [ebpf/secret_monitor.o]\n", c.Profile, c.SuiteSHA256, c.GuestSHA256)
}

func suiteManifestFixture() string {
	return "version: 1\nprofile: kvm-suite-amd64-v1\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [ebpf/secret_monitor.o]\ncases: [deny-secret-read, allow-authorized-read]\n"
}

func suiteArchive(t *testing.T, manifest string, extra ...*tar.Header) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: SuiteManifestFile, Mode: 0600, Size: int64(len(manifest)), Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	for _, header := range extra {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func pinSuite(contract Contract, bundle []byte) Contract {
	digest := sha256.Sum256(bundle)
	contract.SuiteSHA256 = hex.EncodeToString(digest[:])
	return contract
}

func TestKVMContractClosedSchemaAndBaseline(t *testing.T) {
	contract, err := DecodeContract([]byte(kvmContractYAML()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(contract, kvmContractFixture()) {
		t.Fatalf("decoded = %#v", contract)
	}
	for name, source := range map[string]string{
		"command":            kvmContractYAML() + "command: true\n",
		"image":              kvmContractYAML() + "image: candidate/harness\n",
		"v1 field":           kvmContractYAML() + "artifact: ''\n",
		"v2 field on v1":     validContract + "suite_sha256: ''\n",
		"missing pin":        strings.ReplaceAll(kvmContractYAML(), "suite_sha256: "+contract.SuiteSHA256+"\n", ""),
		"short pin":          strings.ReplaceAll(kvmContractYAML(), contract.SuiteSHA256, "abc"),
		"uppercase pin":      strings.ReplaceAll(kvmContractYAML(), contract.SuiteSHA256, strings.Repeat("A", 64)),
		"traversal":          strings.ReplaceAll(kvmContractYAML(), BPFArtifact, "../secret"),
		"metadata":           strings.ReplaceAll(kvmContractYAML(), BPFArtifact, "build/.git/config"),
		"duplicate artifact": strings.ReplaceAll(kvmContractYAML(), BPFArtifact, BPFArtifact+", "+BPFArtifact),
		"empty artifacts":    strings.ReplaceAll(kvmContractYAML(), BPFArtifact, ""),
		"profile":            strings.ReplaceAll(kvmContractYAML(), KVMSuiteAMD64Profile, OfflineEBPFProfile),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeContract([]byte(source)); err == nil {
				t.Fatal("unsafe contract admitted")
			}
		})
	}
	required, err := RequiredContract(nil, &contract)
	if err != nil || !reflect.DeepEqual(required, &contract) {
		t.Fatalf("deleted baseline: %v, %v", required, err)
	}
	required.Artifacts[0] = "modified"
	if contract.Artifacts[0] != BPFArtifact {
		t.Fatal("required contract aliases baseline artifacts")
	}
	changed := contract
	changed.GuestSHA256 = strings.Repeat("c", 64)
	if _, err := RequiredContract(&changed, &contract); err == nil {
		t.Fatal("candidate replaced published guest pin")
	}
	changed = contract
	changed.SuiteSHA256 = strings.Repeat("d", 64)
	if _, err := RequiredContract(&changed, &contract); err == nil {
		t.Fatal("candidate replaced published suite pin")
	}
}

func TestKVMSuiteRequiresPinnedBundleAndExactInventory(t *testing.T) {
	bundle := suiteArchive(t, suiteManifestFixture())
	contract := pinSuite(kvmContractFixture(), bundle)
	suite, err := VerifyKVMSuiteBundle(contract, bundle)
	if err != nil {
		t.Fatal(err)
	}
	results := []KVMCaseResult{{ID: "allow-authorized-read", Outcome: "passed"}, {ID: "deny-secret-read", Outcome: "passed"}}
	if err := suite.VerifyInventory(contract, results); err != nil {
		t.Fatal(err)
	}
	for name, results := range map[string][]KVMCaseResult{
		"empty":     nil,
		"missing":   {{ID: "deny-secret-read", Outcome: "passed"}},
		"extra":     {{ID: "deny-secret-read", Outcome: "passed"}, {ID: "allow-authorized-read", Outcome: "passed"}, {ID: "unknown", Outcome: "passed"}},
		"duplicate": {{ID: "deny-secret-read", Outcome: "passed"}, {ID: "deny-secret-read", Outcome: "passed"}},
		"unknown":   {{ID: "deny-secret-read", Outcome: "passed"}, {ID: "unknown", Outcome: "passed"}},
		"skip":      {{ID: "deny-secret-read", Outcome: "skipped"}, {ID: "allow-authorized-read", Outcome: "passed"}},
		"failure":   {{ID: "deny-secret-read", Outcome: "failed"}, {ID: "allow-authorized-read", Outcome: "passed"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := suite.VerifyInventory(contract, results); err == nil {
				t.Fatal("invalid inventory passed")
			}
		})
	}
	if err := (VerifiedKVMSuite{}).VerifyInventory(contract, results); err == nil {
		t.Fatal("unverified suite accepted")
	}
	changed := contract
	changed.GuestSHA256 = strings.Repeat("c", 64)
	if err := suite.VerifyInventory(changed, results); err == nil {
		t.Fatal("suite reused with different contract")
	}
	tampered := bytes.Clone(bundle)
	tampered[512] ^= 1
	if _, err := VerifyKVMSuiteBundle(contract, tampered); err == nil {
		t.Fatal("tampered bundle accepted")
	}
}

func TestKVMSuiteRejectsMalformedPinnedBundle(t *testing.T) {
	manifest := suiteManifestFixture()
	for name, bundle := range map[string][]byte{
		"wrong guest":        suiteArchive(t, strings.ReplaceAll(manifest, strings.Repeat("b", 64), strings.Repeat("c", 64))),
		"wrong artifact":     suiteArchive(t, strings.ReplaceAll(manifest, BPFArtifact, "other.o")),
		"empty cases":        suiteArchive(t, strings.ReplaceAll(manifest, "deny-secret-read, allow-authorized-read", "")),
		"duplicate cases":    suiteArchive(t, strings.ReplaceAll(manifest, "allow-authorized-read", "deny-secret-read")),
		"unknown field":      suiteArchive(t, manifest+"command: true\n"),
		"second document":    suiteArchive(t, manifest+"---\n"),
		"duplicate key":      suiteArchive(t, manifest+"version: 1\n"),
		"anchor":             suiteArchive(t, strings.ReplaceAll(manifest, "cases:", "cases: &cases")),
		"large manifest":     suiteArchive(t, strings.Repeat("x", MaxSuiteManifestBytes+1)),
		"duplicate manifest": suiteArchive(t, manifest, &tar.Header{Name: SuiteManifestFile, Mode: 0600, Format: tar.FormatUSTAR}),
		"symlink":            suiteArchive(t, manifest, &tar.Header{Name: "suite", Linkname: "/etc", Typeflag: tar.TypeSymlink, Format: tar.FormatUSTAR}),
		"traversal":          suiteArchive(t, manifest, &tar.Header{Name: "../suite", Mode: 0600, Format: tar.FormatUSTAR}),
		"trailing archive":   append(suiteArchive(t, manifest), []byte("hidden")...),
		"truncated":          []byte("not tar"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyKVMSuiteBundle(pinSuite(kvmContractFixture(), bundle), bundle); err == nil {
				t.Fatal("malformed pinned bundle admitted")
			}
		})
	}
}
