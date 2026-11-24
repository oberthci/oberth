package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func timeDuration(seconds int) time.Duration { return time.Duration(seconds) * time.Second }
func missingStore(err error) bool {
	return err != nil && publicerr.Classify(err).Code() == publicerr.NotFound
}
func (o *serverOwners) wiringDeps(ctx context.Context) app.Deps {
	return app.Deps{Lifetime: ctx, SourceTimeout: o.sourceTimeout(), PublicURL: o.c.config.Public.URL, Source: cacheSource{o}, Authority: o.authority, Service: o, Runs: o.c.store, Resolver: o, Native: o.native, Preparation: nativebinding.Preparation{HoldTTL: timeDuration(o.c.config.Assist.TTLSeconds), HelperImage: o.settings.HelperImage}, Keeper: o.cache, Deadline: context.WithTimeout, Routes: o.settings.Routes, SourcePolicy: o.settings.SourcePolicy, Version: releaseVersion(), ReleaseFacts: func(ctx context.Context, r app.Request, file string) (admissioncontract.ReleaseFacts, error) {
		return app.VerifiedReleaseFacts(ctx, r, file, o.c.store)
	}}
}
func (o *serverOwners) wiringPart(ctx context.Context) (Part, error) {
	var err error
	o.authority, err = app.NewAuthority(o.authorityConfiguration())
	if err != nil {
		return Part{}, err
	}
	o.wiring, err = app.New(o.wiringDeps(ctx))
	if err != nil {
		return Part{}, err
	}
	o.scheduleConfig()
	return Part{Name: "submission wiring", Recover: o.wiring.Recover, Close: o.wiring.Close}, nil
}
func (o *serverOwners) authorityConfiguration() app.AuthorityConfig {
	c := o.c.config
	return app.AuthorityConfig{ArgoNamespace: c.ArgoNamespace, ServerNamespace: c.Namespace, Namespace: c.PipelineNamespace, SecretstoreAddress: c.SecretStore.Address, ImagePrefixes: o.settings.ImagePrefixes, Defaults: o.settings.Defaults, Grants: o.c.store.Grants, Templates: o.template, EnsureAccount: o.native.EnsureAccount}
}
func (o *serverOwners) template(ctx context.Context, name string) (*wf.WorkflowTemplate, error) {
	return o.cluster.Argo.ArgoprojV1alpha1().WorkflowTemplates(o.c.config.PipelineNamespace).Get(ctx, name, meta.GetOptions{})
}
func (o *serverOwners) Decide(ctx context.Context, u service.RefUpdate) (service.Decision, error) {
	return o.service.Decide(ctx, u)
}
func (o *serverOwners) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	return o.service.Accept(ctx, b)
}
func (o *serverOwners) ResolveStart(ctx context.Context, q service.StartRequest) (app.Request, error) {
	r, err := o.c.store.Repository(ctx, q.Request.Repo, nil)
	if err != nil {
		return app.Request{}, err
	}
	ref := sourcecontract.Ref{Name: q.Request.Ref, ObjectSHA: q.Request.ObjectSHA}
	if q.Request.CommitSHA != nil {
		ref.CommitSHA = *q.Request.CommitSHA
	}
	if ref.ObjectSHA == "" {
		if ref.Name == "" {
			ref.Name = o.settings.DefaultBranch
		}
		ref, err = o.resolveInput(ctx, r.Canonical, ref.Name)
		if err != nil {
			return app.Request{}, err
		}
	}
	trigger := q.Request.Trigger
	if trigger == "" {
		trigger = model.TriggerBranch
		if strings.HasPrefix(ref.Name, "refs/tags/") {
			trigger = model.TriggerTag
		}
	}
	request := app.Request{RepositoryID: r.ID, Repository: r.Canonical, RunID: sourcecontract.Digest([]byte(q.Request.ReceiveID))[:32], Ref: ref, Trigger: trigger, ScheduleEntry: q.Request.ScheduleEntry, Assist: q.Request.Assist, Actor: q.Request.Actor.Clone()}
	if err := o.resolveRelease(ctx, r, &request, q.RerunOf); err != nil {
		return app.Request{}, err
	}
	return request, nil
}
func (o *serverOwners) ReleaseTag(ctx context.Context, r stateport.Repository, ref gitport.Ref) (gitport.Ref, error) {
	if _, err := o.settings.Routes.Resolve(r.Canonical, strings.TrimPrefix(ref.Name, "refs/tags/")); err != nil {
		return gitport.Ref{}, err
	}
	head, reachable, err := o.cache.DefaultAncestor(ctx, sourcecontract.Revision{Repository: r.Canonical, Commit: ref.CommitSHA})
	if err != nil {
		return gitport.Ref{}, err
	}
	if !reachable {
		return gitport.Ref{}, publicerr.New(publicerr.Forbidden, "Release commit is outside default history.", "Release a tested commit from the default branch.", nil)
	}
	return head, nil
}
func (o *serverOwners) ListRefs(ctx context.Context, repo string) ([]gitport.Ref, error) {
	if err := o.cache.Init(ctx, repo); err != nil {
		return nil, err
	}
	return o.cache.ListRefs(ctx, repo)
}
func (o *serverOwners) Cancel(ctx context.Context, r stateport.Run, force bool) error {
	b, found, err := o.native.Find(ctx, r)
	if err != nil || !found {
		return err
	}
	if force {
		return o.native.Terminate(ctx, b)
	}
	return o.native.Stop(ctx, b)
}
func (o *serverOwners) Locks(ctx context.Context) ([]service.Lock, error) {
	repos, err := o.repositories(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, r := range repos {
		names[strconv.FormatInt(r.ID, 10)] = r.Canonical
	}
	rows, err := o.cluster.Argo.ArgoprojV1alpha1().Workflows(o.c.config.PipelineNamespace).List(ctx, meta.ListOptions{LabelSelector: nativebinding.EngineLabel + "=" + nativebinding.EngineValue})
	if err != nil {
		return nil, err
	}
	facts, err := argojob.LockFacts(ctx, rows.Items)
	if err != nil {
		return nil, err
	}
	locks := runLocks(facts, names, lockClearCommand(o.c.config.ArgoNamespace))
	if err := o.lockLimits(ctx, locks); err != nil {
		return nil, err
	}
	return locks, nil
}
