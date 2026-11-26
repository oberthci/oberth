package app

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (s *Submitter) Check(ctx context.Context, r Request) (*admissioncontract.PreparedAdmission, error) {
	r.RunID = ""
	run, err := s.admit(ctx, r, s.d.Source, admitOptions{mode: admissioncontract.ModeInspect})
	return run.Prepared, err
}

type admitOptions struct {
	mode   admissioncontract.Mode
	file   string
	reason string
}

func (s *Submitter) admit(ctx context.Context, r Request, source Source, opts admitOptions) (stateport.Acceptance, error) {
	if err := validRequest(r, opts.mode); err != nil {
		return stateport.Acceptance{}, err
	}
	file, manual := requestedFile(r, opts)
	if manual {
		r.File = file
	}
	selected, release, err := workflowpolicy.Select(s.d.Routes, workflowpolicy.SelectionRequest{
		Repository: r.Repository, Ref: r.Ref.Name, Tag: r.Trigger == model.TriggerTag, Release: r.Release, File: file, Manual: manual,
	})
	if err != nil {
		return stateport.Acceptance{}, workflowSelectionError(err)
	}
	if opts.file != "" {
		if release {
			r.Trigger = model.TriggerTag
		}
	}
	scope := Scope{Actor: r.Actor.Clone(), Mode: opts.mode, RepositoryID: r.RepositoryID, Preparation: s.d.Preparation}
	scope.Trigger = string(r.Trigger)
	scope.Repository, scope.File, scope.Release, scope.Assist = r.Repository, selected, release, r.Assist && assistEnabled
	q, policy, err := s.d.Authority.Load(ctx, scope)
	if err != nil {
		return stateport.Acceptance{}, refusal("decision", err)
	}
	revision := sourcecontract.Revision{Repository: r.Repository, Commit: r.Ref.CommitSHA}
	f, err := source.ReadFile(ctx, revision, selected)
	if err != nil {
		return stateport.Acceptance{}, fileError(r, selected, err)
	}
	q = q.Clone()
	q.Mode, q.IdentityRequest.Mode = opts.mode, opts.mode
	q.Repository, q.RunID = r.Repository, r.RunID
	q.Ref, q.CommitSHA, q.ObjectSHA = r.Ref.Name, r.Ref.CommitSHA, r.Ref.ObjectSHA
	q.Trigger, q.ScheduleEntry = r.Trigger, r.ScheduleEntry
	q.SelectedFile, q.FileBlob, q.Document = selected, f.Blob, f.Bytes
	q.SourceReceipt = sourcecontract.Receipt{Version: 1, Revision: revision, ObjectSHA: r.Ref.ObjectSHA, File: selected, Blob: f.Blob, DocumentDigest: sourcecontract.Digest(f.Bytes)}
	hooks := s.admissionHooks(r, source, scope)
	p, err := argoworkflow.Prepare(ctx, q, policy, hooks)
	if err != nil {
		return stateport.Acceptance{}, admissionFailure(err, r.Ref.Name, selected)
	}
	run := stateport.Acceptance{Request: ciRequest(r), Release: scope.Release, Prepared: p}
	if opts.mode == admissioncontract.ModeSubmit {
		run.Actions = []model.AuditAction{audit(run, opts.reason)}
	}
	return run, nil
}

func workflowSelectionError(err error) *Refusal {
	var invalid *workflowpolicy.InvalidError
	if errors.As(err, &invalid) {
		return refusal("workflow", publicerr.New(publicerr.InvalidInput, invalid.Reason(), invalid.Remedy(), nil))
	}
	return refusal("workflow", err)
}
func validRequest(r Request, mode admissioncontract.Mode) error {
	_, err := reponame.ParseRepository(r.Repository)
	if err != nil || r.RepositoryID < 1 || !r.Trigger.Valid() || r.Ref.Validate() != nil || !r.Actor.CanTouchRepository(r.Repository) || mode == admissioncontract.ModeSubmit && r.RunID == "" {
		return refusal("request", err)
	}
	return nil
}
func fileError(r Request, file string, err error) *Refusal {
	code := "read"
	var absent *gitport.AbsentError
	if errors.As(err, &absent) && absent.Path == file {
		code = "missing"
	}
	e := refusal(code, err)
	e.Ref, e.File = r.Ref.Name, file
	return e
}

// PendingGrants reads configured files at one default head, without partial success.
func (s *Submitter) PendingGrants(ctx context.Context, r Request) ([]admissioncontract.Finding, error) {
	snapshot, err := s.d.Source.Checkout(ctx, r.Repository, sourcecontract.Ref{})
	if err != nil {
		return nil, err
	}
	r.Ref = snapshot.DefaultBranch
	var out []admissioncontract.Finding
	for _, file := range append([]string{workflowpolicy.BuildFile}, s.d.Routes.Files(r.Repository)...) {
		run, err := s.admit(ctx, r, s.d.Source, admitOptions{mode: admissioncontract.ModeInspect, file: file})
		var e *Refusal
		if errors.As(err, &e) && e.Code == "missing" {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, f := range run.Prepared.View().Findings {
			if f.Code == "grant" {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

func admissionFailure(err error, ref, file string) *Refusal {
	e := refusal("unavailable", err)
	var bound *Refusal
	if errors.As(err, &bound) {
		copy := *bound
		e = &copy
	}
	var authored *argoworkflow.AdmissionError
	if errors.As(err, &authored) {
		e = refusal("prepare", err)
		e.Problems = append([]admissioncontract.Finding(nil), authored.Findings...)
		e.Detail = authored.Detail
	}
	e.Ref = ref
	e.File = file
	if authored != nil {
		e.listFindings()
	}
	return e
}

func ciRequest(r Request) model.CIRequest {
	return model.CIRequest{Repo: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: &r.Ref.CommitSHA, Actor: r.Actor.Clone(), Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, Assist: r.Assist, RequestedFile: r.File}
}

func (s *Submitter) DefaultBranch(ctx context.Context, repo string) (string, error) {
	head, err := s.d.Source.Checkout(ctx, repo, sourcecontract.Ref{})
	return head.DefaultBranch.Name, err
}

// DefaultFile retains the fresh default identity on a typed file absence.
func (s *Submitter) DefaultFile(ctx context.Context, repo, path string) (ref gitport.Ref, file gitport.File, err error) {
	var head sourcecontract.Export
	if head, err = s.d.Source.Checkout(ctx, repo, sourcecontract.Ref{}); err == nil {
		ref = head.DefaultBranch
		file, err = s.d.Source.ReadFile(ctx, sourcecontract.Revision{Repository: repo, Commit: ref.CommitSHA}, path)
	}
	return ref, file, err
}

func (s *Submitter) admissionHooks(r Request, source Source, scope Scope) admissioncontract.PrepareHooks {
	preparation := scope.Preparation
	preparation.RepositoryID, preparation.Ref, preparation.Trigger = r.RepositoryID, r.Ref, string(r.Trigger)
	hooks := s.d.Native.Hooks(preparation)
	binder := s.binder(r, source)
	hooks.PrepareSources, hooks.BindSource = binder.prepare, binder.bind
	if r.Release && scope.Release && s.d.ReleaseFacts != nil {
		hooks.ReleaseFacts = func(ctx context.Context) (admissioncontract.ReleaseFacts, error) {
			return s.d.ReleaseFacts(ctx, r, scope.File)
		}
	}
	return hooks
}

func requestedFile(r Request, opts admitOptions) (string, bool) {
	if r.Original != nil && !r.Original.Release {
		r.File = r.Original.Origin.RequestedFile
		if selected := r.Original.Admission.SelectedFile; selected != "" && selected != workflowpolicy.BuildFile {
			r.File = selected
		}
	}
	file := opts.file
	if r.File != "" {
		file = r.File
	}
	return file, r.File != ""
}
