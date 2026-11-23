package main

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sshserver"
	"golang.org/x/crypto/ssh"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (o *serverOwners) sourcePart(ctx context.Context) (Part, error) {
	server, err := sshserver.New(sshserver.Config{Signer: o.c.hostKey, Lookup: o.sshIdentity, Run: o.gitSession, Now: o.c.clock.Now})
	if err != nil {
		return Part{}, err
	}
	o.c.ssh = server.Serve
	authentication := sourcedelivery.KubernetesAuth{Review: func(ctx context.Context, r *auth.TokenReview) (*auth.TokenReview, error) {
		return o.cluster.Kube.AuthenticationV1().TokenReviews().Create(ctx, r, meta.CreateOptions{})
	}, Pod: func(ctx context.Context, ns, name string) (*core.Pod, error) {
		return o.cluster.Kube.CoreV1().Pods(ns).Get(ctx, name, meta.GetOptions{})
	}}
	o.c.source = sourcedelivery.New(sourcedelivery.Config{Authenticate: authentication.Authenticate, Run: o.sourceRun, Stage: o.cache.Stage, Now: o.c.clock.Now, SourceClientBurst: o.settings.SourceClientBurst})
	return Part{Name: "source"}, ctx.Err()
}
func (o *serverOwners) sourceRun(ctx context.Context, id string) (sourcedelivery.Run, error) {
	r, err := o.c.store.Run(ctx, id)
	return sourcedelivery.Run{Admission: r.Admission, Namespace: r.Admission.Identity.Namespace, Workflow: r.Origin.WorkflowName, UID: r.Origin.WorkflowUID, Terminal: terminalRun(r.Status)}, err
}
func (o *serverOwners) sshIdentity(ctx context.Context, key ssh.PublicKey) (sshserver.Identity, error) {
	who, err := (currentAuthority{o}).byFingerprint(ctx, ssh.FingerprintSHA256(key), "ssh")
	if err != nil {
		problem := publicerr.Classify(err)
		if problem.Code() == publicerr.NotFound || problem.Code() == publicerr.Forbidden {
			return sshserver.Identity{}, &sshserver.UnregisteredError{}
		}
		return sshserver.Identity{}, err
	}
	return sshserver.Identity{Actor: who.Identity, Fingerprint: who.Fingerprint, CanAccess: who.CanTouchRepository}, nil
}
func (o *serverOwners) gitSession(ctx context.Context, s sshserver.Session) error {
	who, err := (currentAuthority{o}).byFingerprint(ctx, s.Identity.Fingerprint, "ssh")
	if err != nil {
		return err
	}
	repo, err := service.ResolveRepository(ctx, o.c.store, who, s.Command.Repository)
	if err != nil {
		if publicerr.Classify(err).Code() == publicerr.NotFound {
			return publicerr.New(publicerr.NotFound, "Repository not found.", "This repository is not registered here; run oberth repo list to see registered repositories.", nil)
		}
		return err
	}
	if repo.Archived {
		return publicerr.New(publicerr.NotFound, "Repository is not available.", "Ask an administrator to register an active repository.", nil)
	}
	if err := o.cache.Init(ctx, s.Command.Repository); err != nil {
		return err
	}
	if s.Command.Service == "git-upload-pack" {
		return o.cache.Upload(ctx, s.Command.Repository, gitcache.ServiceIO{Input: s.Input, Output: s.Output, Diagnostics: s.Diagnostics, Begin: s.Begin, End: s.End})
	}
	policy, err := (currentAuthority{o}).KeyPolicy(ctx, who)
	if err != nil {
		return err
	}
	head, err := o.receiveHead(ctx, s.Command.Repository)
	if err != nil {
		return err
	}
	return o.cache.Receive(ctx, gitcache.ReceiveRequest{Repository: s.Command.Repository, Actor: who.Identity, Fingerprint: who.Fingerprint, AllowDefault: !policy.Agent, AlwaysRun: !policy.Agent, Admission: gitcache.ReleaseAdmission{DefaultBranch: head.Name, SHA: head.CommitSHA}, Input: s.Input, Output: s.Output, Diagnostics: s.Diagnostics, Begin: s.Begin, End: s.End, Deliver: o.deliver})
}

func (o *serverOwners) receiveHead(ctx context.Context, repository string) (gitport.Ref, error) {
	head, err := o.cache.ResolveRef(ctx, repository, o.settings.DefaultBranch)
	var absent *gitport.AbsentError
	if !errors.As(err, &absent) {
		return head, err
	}
	liveErr := o.cache.DefaultHead(ctx, repository)
	var empty *gitport.EmptyRepositoryError
	if errors.As(liveErr, &empty) {
		return gitport.Ref{Name: o.settings.DefaultBranch}, nil
	}
	if liveErr != nil {
		return head, liveErr
	}
	return head, err
}
