package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

const statusProbeTimeout = 5 * time.Second

func (a serverAdministration) statusSnapshot(ctx context.Context, action string) ([]apicontract.PartStatus, *[]apicontract.EffectiveSetting) {
	if action != "system_status" {
		return a.statusParts(ctx), nil
	}
	var cap, backup settingsRows
	var probes sync.WaitGroup
	probes.Go(func() { cap.runCap(ctx, a.owner) })
	probes.Go(func() { backup.backupJob(ctx, a.owner) })
	parts := a.statusParts(ctx)
	probes.Wait()
	settings := a.effectiveSettings(ctx, parts, append(cap.values, backup.values...))
	return parts, &settings
}

func (a serverAdministration) statusParts(ctx context.Context) []apicontract.PartStatus {
	c := a.owner.c
	if c.lifecycle == nil {
		return []apicontract.PartStatus{}
	}
	rows := c.lifecycle.status()
	c.health.mu.RLock()
	fresh := !c.health.success.IsZero() && c.clock.Now().Sub(c.health.success) < 90*time.Second
	c.health.mu.RUnlock()
	for i := range rows {
		a.inspectPartStatus(ctx, &rows[i], fresh)
	}
	return rows
}

func (a serverAdministration) inspectPartStatus(ctx context.Context, p *apicontract.PartStatus, fresh bool) {
	c := a.owner.c

	switch p.Name {
	case "pending pushes":
		a.receiveStatus(ctx, p)
	case "cluster":
		if p.Ready && (c.health.watch == nil || !c.health.watch()) {
			p.Ready, p.DisplayReason = false, "watch disconnected"
		}
	case "secret store":
		if p.Ready {
			a.secretStoreStatus(ctx, p)
		}
	case "store":
		if p.Ready && !fresh {
			p.Ready, p.DisplayReason = false, "database unavailable"
		}
	}
}

func (l *lifecycle) status() []apicontract.PartStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := make([]apicontract.PartStatus, 0, len(l.parts))
	for _, p := range l.parts {
		row := apicontract.PartStatus{Name: p.Name, Ready: p.ready, DisplayReason: "ready"}
		switch {
		case l.closing:
			row.Ready, row.DisplayReason = false, "stopping"
		case p.err != nil:
			row.Ready, row.DisplayReason = false, "part stopped"
		case !p.ready:
			row.DisplayReason = "starting"
		}
		rows = append(rows, row)
	}
	return rows
}

func (a serverAdministration) receiveStatus(ctx context.Context, p *apicontract.PartStatus) {
	p.DisplayReason = "ready"
	pending, err := a.owner.cache.PendingReceives(ctx)
	if err != nil {
		p.DisplayReason = "receive inspection unavailable"
		return
	}
	for _, r := range pending {
		if r.Problem != "" || r.State == "corrupt" {
			p.DisplayReason = "receives set aside; inspect outbox"
			return
		}
	}
}

func (a serverAdministration) secretStoreStatus(ctx context.Context, p *apicontract.PartStatus) {
	probe, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	if err := a.owner.c.storeWriter.Client.Readiness(probe); err != nil {
		p.Ready, p.DisplayReason = false, "secret store unavailable"
		var refusal *publicerr.Error
		if errors.As(err, &refusal) && refusal.Reason() == "Secret store is sealed." {
			p.DisplayReason, p.Remedy = "secret store sealed", refusal.Remedy()
		}
	}
}
