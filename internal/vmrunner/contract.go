package vmrunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	ContractFile       = ".oberth/tests.yaml"
	MaxContractBytes   = 4 << 10
	OfflineEBPFProfile = "ebpf-offline-amd64-v1"
	KVMSuiteProfile    = "kvm-suite-amd64-v1"
	BPFArtifact        = "ebpf/secret_monitor.o"
	BurnName           = "trusted-tests"

	// V2 profile resource ceilings.
	MaxProfileCPUCores  = 4
	MinProfileMemoryMiB = 512
	MaxProfileMemoryMiB = 16384
	MinProfileTimeout   = 1 * time.Minute
	MaxProfileTimeout   = 2 * time.Hour
)

// knownV2Profiles is the closed vocabulary of version 2 profile names.
// A repository cannot invent new profiles; they require a product change.
var knownV2Profiles = map[string]bool{
	KVMSuiteProfile: true,
}

// Contract is deliberately closed. New profiles and inputs require a product
// change; a branch cannot choose images, identities, commands or resource limits.
type Contract struct {
	Version  int    `json:"version" yaml:"version"`
	Profile  string `json:"profile" yaml:"profile"`
	Artifact string `json:"artifact" yaml:"artifact"`

	// V2 fields — zero-valued for version 1 contracts.
	GuestImage string            `json:"guest_image,omitempty"`
	Resources  ContractResources `json:"resources,omitempty"`
	Timeout    time.Duration     `json:"timeout,omitempty"`
}

// ContractResources bounds the trusted test execution's resource allocation.
type ContractResources struct {
	CPUCores  int `json:"cpu_cores" yaml:"cpu_cores"`
	MemoryMiB int `json:"memory_mib" yaml:"memory_mib"`
}

// contractV1YAML is the strict decode target for version 1 contracts.
type contractV1YAML struct {
	Version  int    `yaml:"version"`
	Profile  string `yaml:"profile"`
	Artifact string `yaml:"artifact"`
}

// contractV2YAML is the strict decode target for version 2 contracts.
type contractV2YAML struct {
	Version    int                   `yaml:"version"`
	Profile    string                `yaml:"profile"`
	Artifact   string                `yaml:"artifact"`
	GuestImage string                `yaml:"guest_image"`
	Resources  contractResourcesYAML `yaml:"resources"`
	Timeout    string                `yaml:"timeout"`
}

type contractResourcesYAML struct {
	CPUCores  int `yaml:"cpu_cores"`
	MemoryMiB int `yaml:"memory_mib"`
}

// DecodeContract accepts exactly one bounded document. Absence is represented
// by a nil Contract at the caller; an existing empty file is an error.
func DecodeContract(source []byte) (Contract, error) {
	if len(source) == 0 || len(source) > MaxContractBytes {
		return Contract{}, errors.New("trusted test contract must contain 1..4096 bytes")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Contract{}, fmt.Errorf("decode trusted test contract: %w", err)
	}
	if err := plainContractNode(&document); err != nil {
		return Contract{}, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Contract{}, errors.New("trusted test contract must contain exactly one document")
	}

	// Two-pass decode: extract version first, then dispatch.
	version, err := decodeContractVersion(source)
	if err != nil {
		return Contract{}, err
	}
	switch version {
	case 1:
		return decodeContractV1(source)
	case 2:
		return decodeContractV2(source)
	default:
		return Contract{}, fmt.Errorf("trusted test contract version %d is not supported; known versions are 1 and 2", version)
	}
}

func decodeContractVersion(source []byte) (int, error) {
	var header struct {
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(source, &header); err != nil {
		return 0, fmt.Errorf("decode trusted test contract version: %w", err)
	}
	if header.Version == 0 {
		return 0, errors.New("trusted test contract must declare a version")
	}
	return header.Version, nil
}

func decodeContractV1(source []byte) (Contract, error) {
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	var v1 contractV1YAML
	if err := strict.Decode(&v1); err != nil {
		return Contract{}, fmt.Errorf("decode trusted test fields: %w", err)
	}
	if v1.Version != 1 || v1.Profile != OfflineEBPFProfile || v1.Artifact != BPFArtifact {
		return Contract{}, errors.New("trusted test contract requires version 1, profile ebpf-offline-amd64-v1 and artifact ebpf/secret_monitor.o")
	}
	return Contract{Version: 1, Profile: v1.Profile, Artifact: v1.Artifact}, nil
}

func decodeContractV2(source []byte) (Contract, error) {
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	var v2 contractV2YAML
	if err := strict.Decode(&v2); err != nil {
		return Contract{}, fmt.Errorf("decode trusted test fields: %w", err)
	}
	if !knownV2Profiles[v2.Profile] {
		return Contract{}, fmt.Errorf("trusted test contract profile %q is not in the known vocabulary; accepted profiles: %s",
			v2.Profile, knownV2ProfileList())
	}
	if v2.Artifact == "" {
		return Contract{}, errors.New("trusted test contract requires a non-empty artifact path")
	}

	// Validate guest image is digest-pinned.
	if problems := validateGuestImageRef(v2.GuestImage); len(problems) > 0 {
		return Contract{}, fmt.Errorf("trusted test contract guest_image: %w", errors.Join(problems...))
	}

	// Validate resources.
	if v2.Resources.CPUCores < 1 || v2.Resources.CPUCores > MaxProfileCPUCores {
		return Contract{}, fmt.Errorf("trusted test contract resources.cpu_cores must be 1..%d, got %d",
			MaxProfileCPUCores, v2.Resources.CPUCores)
	}
	if v2.Resources.MemoryMiB < MinProfileMemoryMiB || v2.Resources.MemoryMiB > MaxProfileMemoryMiB {
		return Contract{}, fmt.Errorf("trusted test contract resources.memory_mib must be %d..%d, got %d",
			MinProfileMemoryMiB, MaxProfileMemoryMiB, v2.Resources.MemoryMiB)
	}

	// Validate timeout.
	timeout, err := time.ParseDuration(v2.Timeout)
	if err != nil {
		return Contract{}, fmt.Errorf("trusted test contract timeout %q: %w", v2.Timeout, err)
	}
	if timeout < MinProfileTimeout || timeout > MaxProfileTimeout {
		return Contract{}, fmt.Errorf("trusted test contract timeout must be %s..%s, got %s",
			MinProfileTimeout, MaxProfileTimeout, timeout)
	}

	return Contract{
		Version:    2,
		Profile:    v2.Profile,
		Artifact:   v2.Artifact,
		GuestImage: v2.GuestImage,
		Resources:  ContractResources{CPUCores: v2.Resources.CPUCores, MemoryMiB: v2.Resources.MemoryMiB},
		Timeout:    timeout,
	}, nil
}

func knownV2ProfileList() string {
	var names []string
	for name := range knownV2Profiles {
		names = append(names, name)
	}
	return fmt.Sprintf("%v", names)
}

func plainContractNode(node *yaml.Node) error {
	if node == nil || node.Kind == 0 || node.Kind == yaml.AliasNode || node.Anchor != "" {
		return errors.New("trusted test contract requires literal fields without aliases or anchors")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return errors.New("trusted test contract contains an invalid or duplicate key")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := plainContractNode(child); err != nil {
			return err
		}
	}
	return nil
}

// ReadContract never follows a symlink out of the immutable source checkout.
func ReadContract(sourceDir string) (*Contract, error) {
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("open trusted test source: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(ContractFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat trusted test contract: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxContractBytes {
		return nil, errors.New("trusted test contract must be a bounded regular file")
	}
	file, err := root.Open(ContractFile)
	if err != nil {
		return nil, fmt.Errorf("open trusted test contract: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, MaxContractBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read trusted test contract: %w", err)
	}
	contract, err := DecodeContract(body)
	if err != nil {
		return nil, err
	}
	return &contract, nil
}

// RequiredContract prevents a branch from removing its published baseline.
// Versions must match when both exist; a v2 candidate cannot downgrade a v2
// baseline and vice versa.
func RequiredContract(candidate, baseline *Contract) (*Contract, error) {
	if candidate != nil && baseline != nil {
		if !contractEqual(*candidate, *baseline) {
			return nil, errors.New("candidate trusted test contract conflicts with the upstream default requirement")
		}
	}
	selected := candidate
	if selected == nil {
		selected = baseline
	}
	if selected == nil {
		return nil, nil
	}
	copy := *selected
	return &copy, nil
}

// contractEqual compares two contracts for semantic equality.
func contractEqual(a, b Contract) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

func ContractDigest(contract *Contract) string {
	body, _ := json.Marshal(contract)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
