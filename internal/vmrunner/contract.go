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

	"go.yaml.in/yaml/v3"
)

const (
	ContractFile       = ".oberth/tests.yaml"
	MaxContractBytes   = 4 << 10
	OfflineEBPFProfile = "ebpf-offline-amd64-v1"
	BPFArtifact        = "ebpf/secret_monitor.o"
	BurnName           = "trusted-tests"
)

// Contract is deliberately closed. New profiles and inputs require a product
// change; a branch cannot choose images, identities, commands or resource limits.
type Contract struct {
	Version  int    `json:"version" yaml:"version"`
	Profile  string `json:"profile" yaml:"profile"`
	Artifact string `json:"artifact" yaml:"artifact"`
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
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	var contract Contract
	if err := strict.Decode(&contract); err != nil {
		return Contract{}, fmt.Errorf("decode trusted test fields: %w", err)
	}
	if contract.Version != 1 || contract.Profile != OfflineEBPFProfile || contract.Artifact != BPFArtifact {
		return Contract{}, errors.New("trusted test contract requires version 1, profile ebpf-offline-amd64-v1 and artifact ebpf/secret_monitor.o")
	}
	return contract, nil
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
// Version 1 has one fixed profile: any future incompatible union must fail.
func RequiredContract(candidate, baseline *Contract) (*Contract, error) {
	if candidate != nil && baseline != nil && *candidate != *baseline {
		return nil, errors.New("candidate trusted test contract conflicts with the upstream default requirement")
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

func ContractDigest(contract *Contract) string {
	body, _ := json.Marshal(contract)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
