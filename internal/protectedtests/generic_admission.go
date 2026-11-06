package protectedtests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const GenericDeviceResource = "devices.kubevirt.io/kvm"

// GenericAdmission is server-only runtime custody. Intent alone is passed to
// the independently pinned host harness, whose closed wire format is unchanged.
// A capability authorizes resources, not executable test roots or a verdict.
type GenericAdmission struct {
	Version           int                     `json:"version"`
	Intent            Intent                  `json:"intent"`
	Capability        argoworkflow.Capability `json:"capability"`
	SupplementalGroup int64                   `json:"supplemental_group"`
}

func NewGenericAdmission(intent Intent, capability *argoworkflow.Capability, group int64) (GenericAdmission, error) {
	if capability == nil {
		return GenericAdmission{}, errors.New("protected generic execution requires an operator capability")
	}
	intent.Contract.Artifacts = slices.Clone(intent.Contract.Artifacts)
	admission := GenericAdmission{Version: 1, Intent: intent, Capability: cloneCapability(*capability), SupplementalGroup: group}
	return admission, admission.Validate()
}

func (a GenericAdmission) Validate() error {
	if a.Version != 1 || a.Intent.Validate() != nil || a.Capability.Validate() != nil || a.Capability.Namespace != a.Intent.Namespace || a.Capability.ExtendedResources[GenericDeviceResource] < 1 || a.SupplementalGroup <= 0 || a.SupplementalGroup > 2147483647 {
		return errors.New("invalid protected generic execution admission")
	}
	body, err := json.Marshal(a)
	if err != nil || len(body) > 32768 {
		return errors.New("protected generic admission exceeds bound")
	}
	return nil
}

func (a GenericAdmission) Digest() string {
	a.Capability = cloneCapability(a.Capability)
	body, _ := json.Marshal(a)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// MatchCapability compares the complete current authority, including resource
// kinds the current profile does not use. Removed or changed grants refuse;
// reordering an equivalent allowlist does not alter its meaning.
func (a GenericAdmission) MatchCapability(current *argoworkflow.Capability) error {
	if a.Validate() != nil || current == nil || current.Validate() != nil {
		return errors.New("protected generic capability absent or invalid")
	}
	oldBody, _ := json.Marshal(cloneCapability(a.Capability))
	newBody, _ := json.Marshal(cloneCapability(*current))
	if string(oldBody) != string(newBody) {
		return errors.New("protected generic capability changed or revoked")
	}
	return nil
}

func cloneCapability(c argoworkflow.Capability) argoworkflow.Capability {
	c.ResourceKinds = slices.Clone(c.ResourceKinds)
	for i := range c.ResourceKinds {
		c.ResourceKinds[i].Actions = slices.Clone(c.ResourceKinds[i].Actions)
		slices.Sort(c.ResourceKinds[i].Actions)
	}
	slices.SortFunc(c.ResourceKinds, func(a, b argoworkflow.ResourceKindCapability) int {
		return strings.Compare(a.APIVersion+"/"+a.Kind, b.APIVersion+"/"+b.Kind)
	})
	if c.ExtendedResources != nil {
		original := c.ExtendedResources
		c.ExtendedResources = make(map[string]int64, len(original))
		for name, count := range original {
			c.ExtendedResources[name] = count
		}
	}
	return c
}
