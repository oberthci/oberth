package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (o *serverOwners) cachePart(ctx context.Context) (Part, error) {
	if err := os.MkdirAll(o.runtimeDirectory(), 0700); err != nil {
		return Part{}, err
	}
	shim := filepath.Join(o.runtimeDirectory(), "git-ssh")
	if err := installGitShim(shim, o.platform.executable); err != nil {
		return Part{}, err
	}
	data := filepath.Dir(o.c.config.DatabaseFile)
	o.c.cacheNotices = make(chan cacheNotice, 128)
	notices := newRefNotices(ctx, func(ctx context.Context, repo string, refs []string) error {
		return o.c.schedules.RefsChanged(ctx, repo, refs)
	}, o.notice)
	cache, err := gitcache.New(ctx, gitcache.Config{DeliveryContext: ctx, MaxInputBytes: o.settings.ReceiveMaxInputBytes, Log: o.cacheObservation, AdmissionMetric: o.admissionMetric, Root: filepath.Join(data, "git"), Scratch: filepath.Join(data, "scratch"), Binary: o.settings.GitBinary, HookBinary: o.platform.executable, Now: o.c.clock.Now, Upstream: o.upstream, LiveOwners: o.liveOwners, AdmissionTimeout: timeDuration(o.settings.AdmissionTimeoutSeconds), PreReceive: o.gate, AbortReceive: func(id string) { o.wiring.Forget(id) }, RefChanged: notices.changed})
	if err != nil {
		return Part{}, errors.Join(err, notices.close(context.WithoutCancel(ctx)))
	}
	o.cache, o.notices = cache, notices
	o.checks = &app.Checks{Git: probeGit{cache}, Deadline: context.WithTimeout}
	o.c.cacheGC = o.maintainReceives
	o.c.replayPending = o.recoverPendingReceives
	return Part{Name: "Git cache", Shutdown: stopResources, Close: func(ctx context.Context) error {
		return errors.Join(notices.close(ctx), cache.Close())
	}}, ctx.Err()
}

func installGitShim(path, executable string) error {
	if target, err := os.Readlink(path); err == nil {
		if target == executable {
			return nil
		}
		return serverProblem("config", "Git shim belongs to another executable: "+path, nil)
	}
	return os.Symlink(executable, path)
}
func (o *serverOwners) upstream(ctx context.Context, canonical string) (gitcache.Remote, error) {
	name, err := reponame.ParseRepository(canonical)
	if err != nil {
		return gitcache.Remote{}, err
	}
	u, err := o.c.store.Upstream(ctx, name.Upstream)
	if err != nil {
		return gitcache.Remote{}, err
	}
	base, err := name.URL(u.BaseURL)
	if err != nil {
		return gitcache.Remote{}, err
	}
	remote := gitcache.Remote{DefaultBranch: o.settings.DefaultBranch}
	base.Scheme = "ssh"
	if base.User == nil {
		base.User = url.User("git")
	}
	base.Host = sshURLHost(base)
	remote.URL = base.String()
	remote.SSHBinary = filepath.Join(o.runtimeDirectory(), "git-ssh")
	remote.SSHConfig, err = (serverAdministration{o}).configure(ctx, u)
	if err != nil {
		return remote, err
	}
	r, err := o.c.store.Repository(ctx, canonical, nil)
	if err == nil {
		remote.RepositoryID = r.ID
	} else if !missingStore(err) {
		return remote, err
	}
	return remote, nil
}
func (o *serverOwners) liveOwners(ctx context.Context, cursor string) (gitcache.OwnerPage, error) {
	before, err := strconv.ParseInt("0"+cursor, 10, 64)
	if err != nil {
		return gitcache.OwnerPage{}, err
	}
	rows, err := o.c.store.Runs(ctx, stateport.RunFilter{Before: before, Limit: 128})
	out := gitcache.OwnerPage{}
	for _, r := range rows {
		if !terminalRun(r.Status) {
			full, err := o.c.store.Run(ctx, r.ID)
			if err != nil {
				return out, err
			}
			out.Owners = append(out.Owners, gitcache.Owner{ID: r.ID, Bindings: full.Admission.SourceBindings})
		}
	}
	if len(rows) == 128 {
		out.Next = strconv.FormatInt(rows[len(rows)-1].Sequence, 10)
	}
	return out, err
}
func terminalRun(s stepmodel.RunStatus) bool {
	return s == stepmodel.RunPassed || s == stepmodel.RunFailed || s == stepmodel.RunCancelled
}
func (o *serverOwners) receive(ctx context.Context, e gitcache.ReceiveEvent, source app.Source) (app.Receive, error) {
	repo, err := o.c.store.Repository(ctx, e.Repository, nil)
	if err != nil {
		return app.Receive{}, err
	}
	who, err := (currentAuthority{o}).byFingerprint(ctx, e.Fingerprint, "ssh")
	if err != nil || who.Identity != e.Actor {
		return app.Receive{}, errors.Join(err, serverProblem("config", "receive identity changed", nil))
	}
	r := app.Receive{ID: e.ID, Source: source}
	for _, u := range e.Updates {
		ref := sourcecontract.Ref{Name: u.Ref, ObjectSHA: u.NewSHA}
		if strings.HasPrefix(u.Ref, "refs/heads/") {
			ref.CommitSHA = u.NewSHA
		}
		if u.NewSHA != "" {
			export, err := source.Checkout(ctx, e.Repository, ref)
			if err != nil {
				return r, err
			}
			ref = export.Ref
		}
		request := app.Request{RepositoryID: repo.ID, Repository: e.Repository, RunID: sourcecontract.Digest([]byte(e.ID + "\x00" + u.Ref))[:32], Ref: ref, Trigger: model.TriggerBranch, Actor: who, Assist: e.Options.Assist}
		r.Updates = append(r.Updates, app.RefUpdate{Request: request, PreviousSHA: u.OldSHA, Deleted: u.NewSHA == "", Run: e.Options.Run})
	}
	return r, nil
}
func (o *serverOwners) gate(ctx context.Context, e gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
	for _, update := range e.Updates {
		if update.Ref == o.settings.DefaultBranch || strings.HasPrefix(update.Ref, "refs/tags/") {
			if _, err := o.cache.RemoteRef(ctx, e.Repository, o.settings.DefaultBranch); err != nil {
				return err
			}
			break
		}
	}
	r, err := o.receive(ctx, e, s)
	if err != nil {
		return err
	}
	r.Forge = s
	_, err = o.wiring.Gate(ctx, r)
	return err
}
func (o *serverOwners) deliver(ctx context.Context, e gitcache.ReceiveEvent) ([]string, error) {
	if err := o.receiveDeliveryAuthority(ctx, e); err != nil {
		return nil, err
	}
	if _, err := o.c.store.AcceptedRuns(ctx, e.ID); err == nil {
		result, err := o.wiring.Deliver(ctx, app.Receive{ID: e.ID})
		return result.Lines(), err
	} else if !missingStore(err) {
		return nil, err
	}
	r, err := o.receive(ctx, e, cacheSource{o})
	if err != nil {
		return nil, err
	}
	result, err := o.wiring.Deliver(ctx, r)
	return result.Lines(), err
}

type cacheSource struct{ owner *serverOwners }

func (s cacheSource) ReadFile(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
	return s.owner.cache.ReadFile(ctx, r, path)
}
func (s cacheSource) Checkout(ctx context.Context, repo string, ref sourcecontract.Ref) (sourcecontract.Export, error) {
	if err := s.owner.cache.Init(ctx, repo); err != nil {
		return sourcecontract.Export{}, err
	}
	return s.owner.cache.Checkout(ctx, repo, ref)
}
func (s cacheSource) Pin(ctx context.Context, r sourcecontract.Revision) (int64, bool, error) {
	return s.owner.cache.Pin(ctx, r)
}

func (s cacheSource) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if err := s.owner.cache.Init(ctx, repo); err != nil {
		return gitport.Ref{}, err
	}
	if err := s.owner.cache.RefreshRead(ctx, repo, nil); err != nil {
		return gitport.Ref{}, err
	}
	return s.owner.cache.ResolveRef(ctx, repo, ref)
}

func (o *serverOwners) runtimeDirectory() string {
	return filepath.Join(filepath.Dir(o.c.config.DatabaseFile), "runtime")
}

func (o *serverOwners) admissionMetric(string) { o.c.health.admissionTimeouts.Add(1) }

type cacheNotice struct{ reason, detail string }

func (o *serverOwners) cacheObservation(reason, detail string) {
	select {
	case o.c.cacheNotices <- cacheNotice{reason, detail}:
	default:
		o.c.health.cacheNoticeDrops.Add(1)
	}
}
