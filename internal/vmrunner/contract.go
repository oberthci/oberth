package vmrunner

import "github.com/oberthci/oberth/internal/protectedtests"

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

func DecodeContract(source []byte) (Contract, error)   { return protectedtests.DecodeContract(source) }
func ReadContract(sourceDir string) (*Contract, error) { return protectedtests.ReadContract(sourceDir) }
func RequiredContract(candidate, baseline *Contract, authorizeRotation func(Contract) error) (*Contract, error) {
	return protectedtests.RequiredContract(candidate, baseline, authorizeRotation)
}
func ContractDigest(contract *Contract) string { return protectedtests.ContractDigest(contract) }
