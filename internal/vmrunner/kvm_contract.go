package vmrunner

import "github.com/oberthci/oberth/internal/protectedtests"

const MaxKVMArtifacts = protectedtests.MaxKVMArtifacts

func ValidateContract(contract Contract) error { return protectedtests.ValidateContract(contract) }
