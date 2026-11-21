package main

import (
	"context"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func (o *serverOwners) Inspect(ctx context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
	r, err := o.ResolveStart(ctx, service.StartRequest{Request: q.Request})
	if err != nil {
		return apicontract.RunCheckResult{}, err
	}
	if gitoid.Valid(q.Request.Ref) {
		r.Trigger, r.Release = model.TriggerBranch, false
	}
	if q.ReleaseTag != "" {
		r.Ref.Name = "refs/tags/" + q.ReleaseTag
		r.ReleaseHead, err = o.ReleaseTag(ctx, stateport.Repository{Canonical: r.Repository}, r.Ref)
		if err != nil {
			return apicontract.RunCheckResult{}, err
		}
		r.Trigger, r.Release = model.TriggerTag, true
	}
	deps := o.wiringDeps(ctx)
	deps.Source = checkSource{Source: deps.Source, files: q.Files}
	inspector, err := app.New(deps)
	if err != nil {
		return apicontract.RunCheckResult{}, err
	}
	defer func() { o.notice(inspector.Close(ctx)) }()
	prepared, err := inspector.Check(ctx, r)
	if err != nil {
		return apicontract.RunCheckResult{}, err
	}
	v := prepared.View()
	findings := make([]apicontract.Finding, 0, len(v.Findings))
	for _, f := range v.Findings {
		findings = append(findings, apicontract.Finding{Path: f.Path, Code: f.Code, Reason: f.Reason, Remedy: f.Remedy, Command: f.Command})
	}
	releaseTag := ""
	if len(findings) == 0 {
		releaseTag = q.ReleaseTag
	}
	return apicontract.RunCheckResult{ReleaseTag: releaseTag, OK: len(findings) == 0, Repo: r.Repository, Ref: r.Ref.Name, ObjectSHA: &v.ObjectSHA, CommitSHA: &v.CommitSHA, SelectedFile: &v.SelectedFile, SpecDigest: &v.SpecDigest, Plan: &v.Plan, Findings: findings, Checks: []apicontract.Check{{Name: "native identity", State: string(v.Identity.State)}}}, nil
}

type checkSource struct {
	app.Source
	files []apicontract.RunCheckArgsFilesItem
}

func (s checkSource) ReadFile(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
	for _, file := range s.files {
		if file.Path == path {
			data := []byte(file.Content)
			return gitport.File{Path: path, Blob: sourcecontract.Digest(data), Mode: 0100644, Bytes: data}, ctx.Err()
		}
	}
	return s.Source.ReadFile(ctx, r, path)
}
