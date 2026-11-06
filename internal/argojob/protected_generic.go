package argojob

import (
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const genericProtectedAnnotation = "oberth.ci/protected-runtime-admission"

func validateGenericProtectedRequest(r Request) error {
	a := r.GenericProtected
	if a == nil {
		return nil
	}
	if a.Validate() != nil || r.TrustedKVM == nil || protectedtests.IntentDigest(a.Intent) != protectedtests.IntentDigest(*r.TrustedKVM) || r.RepositoryID != a.Intent.RepoID || a.MatchCapability(r.Capability) != nil {
		return errors.New("argojob: generic protected request differs from immutable runtime admission")
	}
	source, err := argoworkflow.Decode(r.Source)
	if err != nil {
		return err
	}
	if _, exists := source.Annotations[argoworkflow.KVMTemplatesAnnotation]; exists {
		return errors.New("argojob: generic protected source cannot request builtin KVM injection")
	}
	return nil
}

func bindGenericProtectedWorkflow(wf *wfv1.Workflow, r Request) error {
	if r.GenericProtected == nil {
		return nil
	}
	peak, err := argoworkflow.DeclaredCapabilityPeak(wf)
	if err != nil || peak == nil {
		return errors.New("argojob: generic protected execution requires a complete final resource graph")
	}
	for name, count := range peak.ExtendedResources {
		if count > r.GenericProtected.Capability.ExtendedResources[name] {
			return errors.New("argojob: protected final device peak exceeds current capability")
		}
	}
	if peak.ExtendedResources[protectedtests.GenericDeviceResource] < 1 {
		return errors.New("argojob: generic protected host device is missing")
	}
	wf.Annotations[genericProtectedAnnotation] = r.GenericProtected.Digest()
	return nil
}

// Only live execution checks current runtime settings. Build can reconstruct a
// persisted old group for exact UID cleanup after configuration changes.
func verifyGenericProtectedRuntime(config Config, r Request) error {
	if r.GenericProtected != nil && config.TrustedKVMGroup != r.GenericProtected.SupplementalGroup {
		return errors.New("argojob: protected runtime group changed after admission")
	}
	return nil
}
