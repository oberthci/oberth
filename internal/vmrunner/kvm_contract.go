package vmrunner

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

const MaxKVMArtifacts = 32

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var artifactPathPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_./-]{0,254}$`)

// ValidateContract enforces the closed wire schemas without granting execution
// authority. Both profiles remain disabled at the scheduler's policy boundary.
func ValidateContract(contract Contract) error {
	switch contract.Version {
	case 1:
		if contract.Profile != OfflineEBPFProfile || contract.Artifact != BPFArtifact || contract.SuiteSHA256 != "" || contract.GuestSHA256 != "" || contract.Artifacts != nil {
			return errors.New("trusted test contract requires version 1, profile ebpf-offline-amd64-v1 and artifact ebpf/secret_monitor.o")
		}
	case 2:
		if contract.Profile != KVMSuiteAMD64Profile || contract.Artifact != "" || !sha256HexPattern.MatchString(contract.SuiteSHA256) || !sha256HexPattern.MatchString(contract.GuestSHA256) {
			return errors.New("trusted test contract v2 requires profile kvm-suite-amd64-v1 and full lowercase suite_sha256 and guest_sha256 pins")
		}
		if err := validateArtifactPaths(contract.Artifacts); err != nil {
			return err
		}
	default:
		return errors.New("trusted test contract version must be 1 or 2")
	}
	return nil
}

func validateArtifactPaths(artifacts []string) error {
	if len(artifacts) == 0 || len(artifacts) > MaxKVMArtifacts {
		return errors.New("trusted test contract requires 1..32 artifact paths")
	}
	seen := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		if !safeSuitePath(artifact) || seen[artifact] {
			return errors.New("trusted test artifact paths must be unique canonical relative paths")
		}
		seen[artifact] = true
	}
	return nil
}

func safeSuitePath(name string) bool {
	if !artifactPathPattern.MatchString(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
