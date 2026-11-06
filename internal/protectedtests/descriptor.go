package protectedtests

import (
	"errors"
	"slices"
)

// Descriptor binds the existing v2 contract to its complete independent
// suite/guest/harness recipe. It is an immutable projection, not a new policy
// file or authority to remove tests.yaml. Runtime evidence remains separate.
type Descriptor struct {
	contract Contract
	recipe   Recipe
}

// BindContract checks the exact old-contract mapping without granting approval.
// Journal readers can reconstruct it without changing stored bytes or needing
// a still-active recipe for cleanup. Execution must use SelectDescriptor;
// publication must also recheck the current contract and recipe approval.
func BindContract(contract Contract, recipe Recipe) (Descriptor, error) {
	if ValidateContract(contract) != nil || contract.Version != 2 || recipe.Validate() != nil ||
		contract.SuiteSHA256 != recipe.SuiteSHA256 || contract.GuestSHA256 != recipe.GuestSHA256 {
		return Descriptor{}, errors.New("protected descriptor requires an exact v2 contract and complete recipe")
	}
	contract.Artifacts = slices.Clone(contract.Artifacts)
	return Descriptor{contract: contract, recipe: recipe}, nil
}

// ValidateRecipes accepts only a bounded, unambiguous operator allowlist.
func ValidateRecipes(recipes []Recipe) error {
	if len(recipes) == 0 || len(recipes) > 128 {
		return errors.New("trusted recipes require 1..128 approved recipes")
	}
	seen := make(map[string]bool, len(recipes))
	for _, recipe := range recipes {
		if err := recipe.Validate(); err != nil {
			return err
		}
		key := recipe.SuiteSHA256 + recipe.GuestSHA256
		if seen[key] {
			return errors.New("duplicate trusted recipe")
		}
		seen[key] = true
	}
	return nil
}

// SelectDescriptor never derives authority from the candidate's pins. Only an
// exact pair in the current operator allowlist can select executable inputs.
func SelectDescriptor(contract Contract, approved []Recipe) (Descriptor, error) {
	if ValidateContract(contract) != nil || contract.Version != 2 {
		return Descriptor{}, errors.New("protected suite executor requires tests.yaml v2")
	}
	if err := ValidateRecipes(approved); err != nil {
		return Descriptor{}, err
	}
	for _, recipe := range approved {
		if recipe.SuiteSHA256 == contract.SuiteSHA256 && recipe.GuestSHA256 == contract.GuestSHA256 {
			return BindContract(contract, recipe)
		}
	}
	return Descriptor{}, errors.New("suite and guest recipe is not operator-approved")
}

// RecipeAllowed rechecks every recipe field, including the host harness and
// download endpoints. Retaining just the suite/guest pair cannot authorize a
// journaled execution after its operator recipe changes or disappears.
func RecipeAllowed(recipe Recipe, approved []Recipe) error {
	if err := ValidateRecipes(approved); err != nil {
		return err
	}
	for _, current := range approved {
		if current == recipe {
			return nil
		}
	}
	return errors.New("journaled trusted recipe has been revoked or changed")
}

func (d Descriptor) Contract() Contract {
	contract := d.contract
	contract.Artifacts = slices.Clone(contract.Artifacts)
	return contract
}

func (d Descriptor) Recipe() Recipe { return d.recipe }

// MatchContract refuses a missing or changed requirement. Reconstructing a
// descriptor is not permission to drop the protected baseline or rotate pins.
func (d Descriptor) MatchContract(contract *Contract) error {
	if _, err := BindContract(d.contract, d.recipe); err != nil {
		return err
	}
	if contract == nil || ValidateContract(*contract) != nil || ContractDigest(contract) != ContractDigest(&d.contract) {
		return errors.New("protected descriptor does not match the required contract")
	}
	return nil
}

// VerifyBundle binds the whole trusted input archive, including its executable
// and all transitive files, before reading the closed case inventory. It does
// not execute candidate source or authorize runtime results.
func (d Descriptor) VerifyBundle(bundle []byte) (VerifiedSuite, error) {
	if err := d.MatchContract(&d.contract); err != nil {
		return VerifiedSuite{}, err
	}
	return VerifySuiteBundle(d.contract, bundle)
}
