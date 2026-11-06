package vmrunner

import "github.com/oberthci/oberth/internal/protectedtests"

const (
	MaxSuiteBundleBytes   = protectedtests.MaxSuiteBundleBytes
	MaxSuiteManifestBytes = protectedtests.MaxSuiteManifestBytes
	MaxSuiteCases         = protectedtests.MaxSuiteCases
	MaxSuiteFiles         = protectedtests.MaxSuiteFiles
	SuiteManifestFile     = protectedtests.SuiteManifestFile
)

type VerifiedKVMSuite = protectedtests.VerifiedSuite
type KVMCaseResult = protectedtests.CaseResult

func VerifyKVMSuiteBundle(contract Contract, bundle []byte) (VerifiedKVMSuite, error) {
	return protectedtests.VerifySuiteBundle(contract, bundle)
}
