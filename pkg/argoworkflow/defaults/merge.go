package defaults

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/util"
)

// MergeTo applies native workflow defaults to private copies; the target wins.
func MergeTo(patch, target *wfv1.Workflow) error { return util.MergeTo(patch, target) }
