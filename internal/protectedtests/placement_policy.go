package protectedtests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PlacementPolicy is prior operator authority for one protected phase. It
// deliberately contains no future host-intent timestamp or digest. The actual
// intent is committed with the runtime admission before any external allocation.
type PlacementPolicy struct {
	BaselineSHA       string   `json:"baseline_sha"`
	Contract          Contract `json:"contract"`
	Recipe            Recipe   `json:"recipe"`
	InventoryDigest   string   `json:"inventory_digest"`
	CaseCount         int      `json:"case_count"`
	CapabilityDigest  string   `json:"capability_digest"`
	SupplementalGroup int64    `json:"supplemental_group"`
	MaxPhaseSeconds   int64    `json:"max_phase_seconds"`
}

func (p PlacementPolicy) Validate() error {
	if _, err := BindContract(p.Contract, p.Recipe); err != nil {
		return err
	}
	if !hexSHA1Pattern.MatchString(p.BaselineSHA) || !sha256HexPattern.MatchString(p.InventoryDigest) || !sha256HexPattern.MatchString(p.CapabilityDigest) || p.CaseCount < 1 || p.CaseCount > MaxSuiteCases || p.SupplementalGroup <= 0 || p.SupplementalGroup > 2147483647 || p.MaxPhaseSeconds <= 0 || p.MaxPhaseSeconds > int64(MaxExecutionDeadline/time.Second) {
		return errors.New("invalid protected placement policy")
	}
	return nil
}

// CapabilityDigest uses the same canonical capability ordering as admission.
func CapabilityDigest(c argoworkflow.Capability) string {
	body, _ := json.Marshal(cloneCapability(c))
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (p PlacementPolicy) Match(a GenericAdmission) error {
	if p.Validate() != nil || a.Validate() != nil || a.Intent.BaselineSHA != p.BaselineSHA || ContractDigest(&a.Intent.Contract) != ContractDigest(&p.Contract) || a.Intent.Recipe != p.Recipe || a.Intent.InventoryDigest != p.InventoryDigest || a.Intent.CaseCount != p.CaseCount || a.SupplementalGroup != p.SupplementalGroup || CapabilityDigest(a.Capability) != p.CapabilityDigest || a.Intent.Deadline.Sub(a.Intent.AdmittedAt) > time.Duration(p.MaxPhaseSeconds)*time.Second {
		return errors.New("protected admission differs from operator placement policy")
	}
	return nil
}
