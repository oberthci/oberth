package main

import (
	"context"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func (o *serverOwners) receiveDeliveryAuthority(ctx context.Context, e gitcache.ReceiveEvent) error {
	err := o.checkReceiveAuthority(ctx, e)
	if err == nil || !publicerr.Permanent(err) {
		return err
	}
	action := stateport.Action{Actor: "oberth", Reason: publicerr.Classify(err).Reason()}
	if err := o.recordReceiveRepair(ctx, action, "abandon", e); err != nil {
		return err
	}
	o.wiring.Forget(e.ID)
	return &gitcache.AbandonReceiveError{}
}

func (o *serverOwners) checkReceiveAuthority(ctx context.Context, e gitcache.ReceiveEvent) error {
	who, err := (currentAuthority{o}).byFingerprint(ctx, e.Fingerprint, "ssh")
	if err != nil {
		return err
	}
	if who.Identity != e.Actor {
		return publicerr.New(publicerr.Forbidden, "Receive identity changed.", "Use the current identity and push again.", nil)
	}
	repo, err := service.ResolveRepository(ctx, o.c.store, who, e.Repository)
	if err != nil {
		return err
	}
	if repo.Archived {
		return publicerr.New(publicerr.Forbidden, "Repository is archived.", "Ask an administrator to restore the repository before pushing again.", nil)
	}
	return nil
}

func (o *serverOwners) recordReceiveRepair(ctx context.Context, action stateport.Action, verb string, e gitcache.ReceiveEvent) error {
	return o.c.store.RecordAdministration(ctx, action, model.AuditAction{Actor: action.Actor, Action: "receive." + verb, ResourceType: "receive", ResourceID: e.ID})
}
