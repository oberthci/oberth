package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/store"
)

const reasonRefusalAudit = "Refusal audit could not be saved."
const remedyRefusalAudit = "Ask an administrator to check the audit store, then try again."
const codeReceiveRefused = "receive_refused"

func (o *serverOwners) refusedReceive(ctx context.Context, event gitcache.ReceiveEvent, cause error) error {
	v := store.ReceiveRefusal{Code: codeReceiveRefused}
	var public *publicerr.Error
	if errors.As(cause, &public) {
		v.Code = string(public.Code())
	}
	var refusal *app.Refusal
	if errors.As(cause, &refusal) {
		v.Code, v.File = refusal.Code, refusal.File
		if v.File == "" {
			v.File = event.Options.File
		}
		if len(refusal.Problems) > 0 {
			finding := refusal.Problems[0]
			v.Code = finding.Code
			_, tail, ok := strings.Cut(finding.Path, "spec.templates[")
			if ok {
				v.Step, _, _ = strings.Cut(tail, "]")
				if strings.HasPrefix(v.Step, `"`) {
					v.Step, _ = strconv.Unquote(v.Step)
				}
			}
		}
	}
	for _, update := range event.Updates {
		v.Updates = append(v.Updates, store.ReceiveRefusalRef{Ref: update.Ref, Old: update.OldSHA, New: update.NewSHA})
	}
	if err := o.c.store.RecordReceiveRefusal(ctx, event.Actor, event.Repository, v); err != nil {
		o.cacheObservation(reasonRefusalAudit, event.Repository)
		return publicerr.New(publicerr.Unavailable, reasonRefusalAudit, remedyRefusalAudit, err)
	}
	return nil
}
