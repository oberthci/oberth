// Package argoworkflow admits repository-authored Argo Workflow YAML.
// The server decodes declarations statically, applies administrator policy,
// and never executes repository code during admission. Shared resource and
// secret-path vocabulary lives in pkg/pipeline.
package argoworkflow

import (
	"errors"
	"fmt"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// The repository-owned pipeline files. A repository declares one Workflow per
// trigger class, so the trust boundary is visible in the filesystem: a branch
// push can only ever run BuildFile, and ReleaseFile only ever runs for a tag
// that already cleared release admission.
const (
	BuildFile   = ".oberth/build.yaml"
	ReleaseFile = ".oberth/release.yaml"
)

const FragmentFile = ".oberth/fragment.yaml"

// MaxSourceBytes bounds one pipeline document before decoding.
const MaxSourceBytes = pipeline.MaxSourceBytes

// ErrUnsupportedTrigger reports that a trigger class has no Argo-authored
// equivalent. Plan and Apply remain Go-path only: their trust model binds a
// sealed artifact to a statically analyzed Go contract, which has no YAML
// counterpart today.
var ErrUnsupportedTrigger = errors.New("argoworkflow: trigger has no Argo-authored pipeline file")

// TriggerFile returns the repository file that defines one trigger's Workflow.
func TriggerFile(trigger pipeline.Trigger) (string, error) {
	switch trigger {
	case pipeline.TriggerCI:
		return BuildFile, nil
	case pipeline.TriggerRelease:
		return ReleaseFile, nil
	default:
		return "", fmt.Errorf("argoworkflow: unknown trigger %q", trigger)
	}
}
