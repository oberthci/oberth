// Package argoworkflow is the admission contract for repositories that author
// their pipeline as real Argo Workflow YAML instead of .oberth/periapsis.go.
//
// It is the structural analogue of pkg/periapsis: the server reads the
// repository's checked-out declaration statically, decides what the run is
// allowed to do, and never evaluates repository code. Where pkg/periapsis
// parses Go declarations with go/ast, this package decodes YAML into the real
// upstream Argo CRD types (wfv1.Workflow) and admits or refuses the result.
//
// The two paths are independent. A repository carries either
// .oberth/periapsis.go or .oberth/build.yaml; nothing in this package changes
// how a periapsis.go repository is admitted or executed.
package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/pkg/periapsis"
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

// MaxSourceBytes bounds one pipeline document. It matches the Go path's bound
// so neither authoring format can present the server with an unbounded read.
const MaxSourceBytes = periapsis.MaxSourceBytes

// ErrUnsupportedTrigger reports that a trigger class has no Argo-authored
// equivalent. Plan and Apply remain Go-path only: their trust model binds a
// sealed artifact to a statically analyzed Go contract, which has no YAML
// counterpart today.
var ErrUnsupportedTrigger = errors.New("argoworkflow: trigger has no Argo-authored pipeline file")

// TriggerFile returns the repository file that defines one trigger's Workflow.
func TriggerFile(trigger periapsis.Trigger) (string, error) {
	switch trigger {
	case periapsis.TriggerCI:
		return BuildFile, nil
	case periapsis.TriggerRelease:
		return ReleaseFile, nil
	default:
		return "", fmt.Errorf("argoworkflow: unknown trigger %q", trigger)
	}
}

// TriggerFiles lists every file that can make a repository Argo-authored, in
// the order dispatch should probe them.
func TriggerFiles() []string { return []string{BuildFile, ReleaseFile} }

// FileTrigger is the inverse of TriggerFile.
func FileTrigger(file string) (periapsis.Trigger, bool) {
	switch strings.TrimSpace(file) {
	case BuildFile:
		return periapsis.TriggerCI, true
	case ReleaseFile:
		return periapsis.TriggerRelease, true
	default:
		return "", false
	}
}
