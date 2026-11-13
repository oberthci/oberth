package app

import "github.com/oberthci/oberth/internal/workflowpolicy"

func (jobs *ArgoJobs) SetResourceBlobs(blobs workflowpolicy.PolicyGit) {
	jobs.resourceBlobs = blobs
	if jobs.routes != nil {
		jobs.routes.local.SetResourceBlobs(blobs)
	}
}
