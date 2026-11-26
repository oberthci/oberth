package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type resumeNative interface {
	PrepareResume(context.Context, stateport.Run, string, string) (nativebinding.ResumePlan, error)
	ApplyResume(context.Context, string) error
	ResumeFailed(context.Context, string, error) error
}

func (s *Submitter) ResumeRun(ctx context.Context, q service.ResumeRequest) (apicontract.ResumeResult, error) {
	var out apicontract.ResumeResult
	if err := s.begin(); err != nil {
		return out, err
	}
	defer s.tasks.Done()
	ctx, cancel := s.submissionContext(ctx)
	defer cancel()
	native, ok := s.d.Native.(resumeNative)
	if !ok {
		return out, refusal("config", nil)
	}
	old := q.Run
	request := model.CIRequest{ReceiveID: q.ID, Repo: old.Repo, Ref: old.Ref, ObjectSHA: q.Commit, CommitSHA: &q.Commit, Actor: q.Actor, Trigger: model.TriggerBranch, Assist: old.Origin.AssistRequested, RequestedFile: old.Origin.RequestedFile}
	r, err := s.d.Resolver.ResolveStart(ctx, service.StartRequest{Request: request, RerunOf: old.ID})
	if err != nil {
		return out, err
	}
	r.Original, r.Assist = &old, old.Origin.AssistRequested
	accepted, err := s.admit(ctx, r, s.d.Source, admitOptions{mode: "submit"})
	if err != nil {
		return out, resumeChanged(err)
	}
	if !argoworkflow.SameExecution(old.Admission, accepted.Prepared.View()) {
		return out, resumeChanged(nil)
	}
	plan, err := native.PrepareResume(ctx, old, r.RunID, q.Commit)
	if err != nil {
		return out, err
	}
	accepted.Prepared, err = argoworkflow.ResumeAdmission(accepted.Prepared, old.Admission)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return out, err
	}
	accepted.Request = request
	accepted.Resume = &model.RunOrigin{Assisted: true, ResumedFrom: old.ID, ResumeUID: old.Origin.NativeUID(), WorkflowName: old.Origin.WorkflowName, ResumePlan: body}

	return s.acceptResume(ctx, accepted, plan, native)
}

func (s *Submitter) acceptResume(ctx context.Context, accepted stateport.Acceptance, plan nativebinding.ResumePlan, native resumeNative) (apicontract.ResumeResult, error) {
	var out apicontract.ResumeResult
	request := accepted.Request
	result, err := s.d.Service.Accept(ctx, stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{accepted}})
	if err != nil {
		return out, err
	}
	if len(result.Runs) != 1 {
		return out, refusal("request", nil)
	}
	out = apicontract.ResumeResult{Run: result.Runs[0].ID, Mode: "retry", Detail: resumeReplyDetail(accepted.Resume.ResumedFrom, request.ObjectSHA, plan)}
	if err = s.keep(ctx, accepted.Prepared.View()); err != nil {
		return out, errors.Join(err, native.ResumeFailed(ctx, out.Run, err))
	}
	err = native.ApplyResume(ctx, out.Run)
	return out, err
}

func resumeChanged(cause error) error {
	return publicerr.New(publicerr.InvalidTransition, "pipeline changed; push for a full run", "Push the fix with -o oberth.run=1 for a full unassisted run.", cause)
}

func resumeReplyDetail(old, commit string, plan nativebinding.ResumePlan) string {
	detail := fmt.Sprintf("from %s at %s; retry; %d steps carried", old, commit[:8], len(plan.Carried))
	if len(plan.SecretSteps) > 0 {
		detail += "; secret steps on F after carried steps: " + strings.Join(plan.SecretSteps, ", ")
	}
	return detail
}
