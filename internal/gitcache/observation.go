package gitcache

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func observationCode(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	var refusal *RefusalError
	var author *gitport.AuthorRefusalError
	if errors.As(err, &refusal) || errors.As(err, &author) {
		return "forbidden"
	}
	return "unavailable"
}

func (c *Cache) observe(o *sourcedelivery.Observation, err error) {
	if c.config.Observe != nil {
		c.config.Observe(finishObservation(o, err))
	}
}

func finishObservation(o *sourcedelivery.Observation, err error) sourcedelivery.Summary {
	s := o.Finish(observationCode(err), err)
	s.Reason = observationReason(err)
	return s
}

func observationReason(err error) string {
	if err == nil {
		return "none"
	}
	if forgeRefused(err) {
		return "forge-refused"
	}
	if forgeSlow(err) {
		return "forge-timeout"
	}
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		switch refusal.Reason() {
		case reasonForgeSilent:
			return "forge-timeout"
		case reasonForgeRefused:
			return "forge-refused"
		case reasonPushBusy, reasonPushWaiting:
			return "contention"
		}
	}
	return "other"
}
