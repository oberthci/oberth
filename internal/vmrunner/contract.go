package vmrunner

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/protectedtests"
)

// The execution adapter retains its existing API and serialized contracts.
// Protected policy and input validation live independently of the VM engine.
const (
	ContractFile         = protectedtests.ContractFile
	MaxContractBytes     = protectedtests.MaxContractBytes
	KVMSuiteAMD64Profile = protectedtests.KVMSuiteAMD64Profile
	OfflineEBPFProfile   = protectedtests.OfflineEBPFProfile
	BPFArtifact          = protectedtests.BPFArtifact
	StageName            = protectedtests.StageName
)

type Contract = protectedtests.Contract

// This legacy filesystem API retains its exact existing retirement exception
// (#1293/#1294). Production protected policy reads through its bounded Git reader.
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
	contract, err := protectedtests.DecodeContract(body)
	if err != nil {
		return nil, err
	}
	return &contract, nil
}

func ContractDigest(contract *Contract) string { return protectedtests.ContractDigest(contract) }
