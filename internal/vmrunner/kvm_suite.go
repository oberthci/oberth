package vmrunner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"

	"go.yaml.in/yaml/v3"
)

const (
	MaxSuiteBundleBytes   = 64 << 20
	MaxSuiteManifestBytes = 16 << 10
	MaxSuiteCases         = 256
	MaxSuiteFiles         = 1024
	SuiteManifestFile     = "manifest.yaml"
)

var suiteCasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// VerifiedKVMSuite can only be constructed from the complete digest-verified
// suite bundle. Its private inventory cannot be replaced by candidate output.
// Verification proves content binding, not publisher authenticity or execution.
type VerifiedKVMSuite struct {
	contractDigest string
	caseIDs        []string
}

type kvmSuiteManifest struct {
	Version     int      `yaml:"version"`
	Profile     string   `yaml:"profile"`
	GuestSHA256 string   `yaml:"guest_sha256"`
	Artifacts   []string `yaml:"artifacts"`
	Cases       []string `yaml:"cases"`
}

// VerifyKVMSuiteBundle hashes all supplied archive bytes before interpreting
// manifest.yaml. V1 bundles are uncompressed tar archives of bounded regular
// files; links, duplicate/noncanonical paths, extensions and trailing data are
// refused. Nothing is extracted to the filesystem or executed. An independently
// authorized recipe remains a prerequisite for runtime use; content hashing
// alone never authorizes execution.
func VerifyKVMSuiteBundle(contract Contract, bundle []byte) (VerifiedKVMSuite, error) {
	if err := ValidateContract(contract); err != nil || contract.Version != 2 {
		return VerifiedKVMSuite{}, errors.New("trusted suite requires a valid v2 contract")
	}
	if len(bundle) == 0 || len(bundle) > MaxSuiteBundleBytes {
		return VerifiedKVMSuite{}, errors.New("trusted suite bundle exceeds bounds")
	}
	digest := sha256.Sum256(bundle)
	if hex.EncodeToString(digest[:]) != contract.SuiteSHA256 {
		return VerifiedKVMSuite{}, errors.New("trusted suite bundle sha256 mismatch")
	}
	input := bytes.NewReader(bundle)
	archive := tar.NewReader(input)
	seen := make(map[string]bool)
	var manifest []byte
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return VerifiedKVMSuite{}, fmt.Errorf("read trusted suite archive: %w", err)
		}
		if len(seen) >= MaxSuiteFiles || header.Typeflag != tar.TypeReg || !safeSuitePath(header.Name) || seen[header.Name] || len(header.PAXRecords) != 0 || header.Format != tar.FormatUSTAR {
			return VerifiedKVMSuite{}, errors.New("trusted suite archive requires unique canonical USTAR regular files")
		}
		seen[header.Name] = true
		if header.Name != SuiteManifestFile {
			continue
		}
		if header.Size < 1 || header.Size > MaxSuiteManifestBytes {
			return VerifiedKVMSuite{}, errors.New("trusted suite manifest exceeds bounds")
		}
		manifest, err = io.ReadAll(io.LimitReader(archive, MaxSuiteManifestBytes+1))
		if err != nil {
			return VerifiedKVMSuite{}, err
		}
	}
	// Archive readers stop at the end marker. Refuse hidden second archives or
	// nonzero trailing bytes even when the complete byte stream has a valid pin.
	for input.Len() > 0 {
		b, err := input.ReadByte()
		if err != nil || b != 0 {
			return VerifiedKVMSuite{}, errors.New("trusted suite archive has trailing data")
		}
	}
	if len(manifest) == 0 {
		return VerifiedKVMSuite{}, errors.New("trusted suite manifest is missing")
	}
	var decoded kvmSuiteManifest
	if err := decodeSuiteManifest(manifest, &decoded); err != nil {
		return VerifiedKVMSuite{}, err
	}
	if decoded.Version != 1 || decoded.Profile != KVMSuiteAMD64Profile || decoded.GuestSHA256 != contract.GuestSHA256 || !reflect.DeepEqual(decoded.Artifacts, contract.Artifacts) {
		return VerifiedKVMSuite{}, errors.New("trusted suite manifest does not match admitted profile, guest or artifacts")
	}
	if len(decoded.Cases) == 0 || len(decoded.Cases) > MaxSuiteCases {
		return VerifiedKVMSuite{}, errors.New("trusted suite requires 1..256 cases")
	}
	cases := make(map[string]bool, len(decoded.Cases))
	for _, id := range decoded.Cases {
		if !suiteCasePattern.MatchString(id) || cases[id] {
			return VerifiedKVMSuite{}, errors.New("trusted suite case IDs must be valid and unique")
		}
		cases[id] = true
	}
	return VerifiedKVMSuite{contractDigest: ContractDigest(&contract), caseIDs: decoded.Cases}, nil
}

func decodeSuiteManifest(source []byte, manifest *kvmSuiteManifest) error {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return err
	}
	if err := plainContractNode(&node); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trusted suite manifest requires one document")
	}
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	return strict.Decode(manifest)
}

type KVMCaseResult struct {
	ID      string
	Outcome string
}

// VerifyInventory compares a trusted harness's outcomes with the pinned suite.
// Logs, guest output and a container-written termination message are not trusted
// outcomes by themselves; the pinned host harness must own protected execution.
func (suite VerifiedKVMSuite) VerifyInventory(contract Contract, results []KVMCaseResult) error {
	if suite.contractDigest == "" || suite.contractDigest != ContractDigest(&contract) || len(suite.caseIDs) == 0 || len(results) != len(suite.caseIDs) {
		return errors.New("trusted case inventory does not match the verified suite")
	}
	remaining := make(map[string]bool, len(suite.caseIDs))
	for _, id := range suite.caseIDs {
		remaining[id] = true
	}
	for _, result := range results {
		if !remaining[result.ID] || result.Outcome != "passed" {
			return errors.New("trusted case inventory contains missing, duplicate, extra or non-passing cases")
		}
		delete(remaining, result.ID)
	}
	return nil
}
