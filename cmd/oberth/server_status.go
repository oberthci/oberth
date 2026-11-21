package main

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

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
		p := &rows[i]
		switch p.Name {
		case "pending pushes":
			a.receiveStatus(ctx, p)
		case "cluster":
			if p.Ready && (c.health.watch == nil || !c.health.watch()) {
				p.Ready, p.DisplayReason = false, "watch disconnected"
			}
		case "store":
			if p.Ready && !fresh {
				p.Ready, p.DisplayReason = false, "database unavailable"
			}
		}
	}
	return rows
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
