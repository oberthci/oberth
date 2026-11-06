package vmrunner

import (
	"regexp"

	"github.com/oberthci/oberth/internal/protectedtests"
)

const MaxKVMArtifacts = protectedtests.MaxKVMArtifacts

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidateContract(contract Contract) error { return protectedtests.ValidateContract(contract) }
