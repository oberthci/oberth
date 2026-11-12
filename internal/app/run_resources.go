package app

import (
	"context"
	"errors"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (jobs *ArgoJobs) SetResourceBlobs(blobs workflowpolicy.PolicyGit) {
	jobs.resourceBlobs = blobs
	if jobs.routes != nil {
		jobs.routes.local.SetResourceBlobs(blobs)
	}
}

// ResourceDeclarations reads only the exact selected commit in the local git
// cache. It returns numeric resource fields, never commands or complete specs.
func (jobs *ArgoJobs) ResourceDeclarations(ctx context.Context, request service.WorkflowRequest) ([]measured.Declaration, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, request.WorkflowName, request.Run.ID)
		if err != nil {
			return nil, err
		}
		return child.ResourceDeclarations(ctx, request)
	}
	resolved, e := jobs.resourceRunSource(ctx, request)
	if e != nil {
		return nil, e
	}
	wf := resolved.Workflow()
	steps, e := argoworkflow.PlannedSteps(wf)
	if e != nil {
		return nil, e
	}
	out := make([]measured.Declaration, 0, len(steps))
	for _, step := range steps {
		t := step.Template
		if t == nil {
			continue
		}
		name := t.Name
		if name == "" {
			name = step.Step
		}
		owner, ownerSHA := argoworkflow.FragmentTemplateOwner(name, resolved.Lock())
		if owner == "" {
			owner = request.Repository.Name
			ownerSHA = request.Run.TestedSHA
			if ownerSHA == "" {
				ownerSHA = request.Run.SHA
			}
		}
		d := measured.Declaration{ResolvedTemplate: name, Stage: measured.NormalizeTemplate(step.Stage), Template: measured.NormalizeTemplate(name), OwnerRepository: owner, SHA: ownerSHA, Resources: map[string]map[string]string{"requests": {}, "limits": {}}}
		var c *corev1.Container
		if t.Container != nil {
			c = t.Container
		} else if t.Script != nil {
			c = &t.Script.Container
		}
		if c != nil {
			for tier, list := range map[string]corev1.ResourceList{"requests": c.Resources.Requests, "limits": c.Resources.Limits} {
				for resource, q := range list {
					if resource == corev1.ResourceCPU || resource == corev1.ResourceMemory || resource == corev1.ResourceEphemeralStorage {
						d.Resources[tier][string(resource)] = q.String()
					}
				}
			}
		}
		if t.ActiveDeadlineSeconds != nil {
			if n, e := strconv.ParseInt(t.ActiveDeadlineSeconds.String(), 10, 64); e == nil {
				d.ActiveDeadlineSeconds = &n
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// resourceRunSource resolves the selected commit without a checkout or execution.
func (jobs *ArgoJobs) resourceRunSource(ctx context.Context, request service.WorkflowRequest) (*argoworkflow.ResolvedRunSource, error) {
	if jobs.resourceBlobs == nil {
		return nil, errors.New("resource declaration git reader unavailable")
	}
	file, e := jobs.runSourceFile(request)
	if e != nil {
		return nil, e
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	repo := model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name)
	source, exists, e := jobs.resourceBlobs.ReadOptionalBlob(ctx, repo, sha, file, argoworkflow.MaxSourceBytes)
	if e != nil {
		return nil, e
	}
	if !exists {
		return nil, errors.New("resource declaration source absent")
	}
	if jobs.targetBinding != nil && targetBytesHash(source) != jobs.targetBinding.SourceDigest {
		return nil, errors.New("app: resource declaration differs from retained source")
	}
	trigger, e := runTrigger(request.Run)
	if e != nil {
		return nil, e
	}
	// Pinned fragments resolve by tag at display time, before a run has a bound
	// receipt. This read is diagnostic; submission still verifies its own binding.
	fragments, e := loadFragments(ctx, jobs.fragments, source, trigger, request.Run.RefKind, request.Run.Trigger)
	if e != nil {
		return nil, e
	}
	kind, e := runKind(request.Run)
	if e != nil {
		return nil, e
	}
	resolved, e := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: sha, File: file, Trigger: kind, ScheduleEntry: request.Run.ScheduleEntry})
	if e != nil {
		return nil, e
	}
	return resolved, nil
}

// RunPodDemand shows effective CPU requests without binding a queued run.
func (jobs *ArgoJobs) RunPodDemand(ctx context.Context, request service.WorkflowRequest) (*argoworkflow.PodDemand, error) {
	if jobs.routes != nil {
		config := jobs.routes.local.config
		repo := model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name)
		for _, target := range jobs.routes.definitions {
			expected, _, ref, _, _, _, _ := target.Config.ExecutionPlacement.Identity()
			if request.Run.RefKind == model.RefBranch && repo == expected && request.Run.Ref == ref {
				config = target.Config
			}
		}
		return jobs.routes.local.queuedCPU(ctx, request, config)
	}
	return jobs.queuedCPU(ctx, request, jobs.config)
}
func (jobs *ArgoJobs) queuedCPU(ctx context.Context, request service.WorkflowRequest, config argojob.Config) (*argoworkflow.PodDemand, error) {
	resolved, err := jobs.resourceRunSource(ctx, request)
	if err != nil {
		return nil, err
	}
	policy := jobs.cpuPolicy(ctx, request.Run, config, resolved.Workflow())
	demand, _ := argojob.SchedulingDemand(resolved.Workflow(), policy)
	return demand, nil
}
