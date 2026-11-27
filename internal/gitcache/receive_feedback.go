package gitcache

import (
	"errors"
	"io"
	"strings"
)

type receiveCompletion struct {
	event    ReceiveEvent
	gate     *receiveGate
	owner    string
	err      error
	returned bool
	accepted bool
	feedback []string
}

func (c *Cache) receiveFeedback(result receiveCompletion, r ReceiveRequest) (string, error) {
	c.writeReceiveNotices(r.Diagnostics, result.feedback)
	if result.gate == nil {
		return "", result.err
	}
	passed := ""
	if result.gate.passed {
		passed = strings.TrimPrefix(result.owner, "receive-")
	}
	if receiveRefusal(result.err) != nil {
		return passed, c.reportReceiveFailure(r.Diagnostics, errors.Join(result.err, result.gate.err))
	}
	if result.gate.err != nil {
		gateErr := result.gate.err
		if result.accepted && result.returned && result.err == nil {
			feedbackErr := writeFeedback(r.Diagnostics, []string{reasonPushAccepted + "."})
			if c.config.Log != nil {
				c.config.Log(reasonPushAccepted, errors.Join(gateErr, feedbackErr).Error())
			}
			return passed, nil
		}
		feedback := reasonGateUnavailable + ". " + remedyGateRetry
		var failure *gateError
		var refusal *RefusalError
		switch {
		case errors.As(gateErr, &failure):
			feedback = failure.Error()
		case errors.As(gateErr, &refusal):
			feedback = refusal.Reason() + ". " + refusal.Remedy()
		}
		gateErr = errors.Join(gateErr, result.err, writeFeedback(r.Diagnostics, []string{feedback}))
		return passed, &reportedError{gateErr}
	}
	return passed, result.err
}

func receiveRefusal(err error) interface {
	Reason() string
	Remedy() string
} {
	var repair *receiveRepairError
	if errors.As(err, &repair) {
		return repair
	}
	var cleanup *receiveCleanupPendingError
	if errors.As(err, &cleanup) {
		return cleanup
	}
	var rollback *receiveRollbackError
	if errors.As(err, &rollback) {
		return rollback
	}
	var refusal *RefusalError
	if errors.As(err, &refusal) && refusal.reason == reasonUnchecked {
		return refusal
	}
	return nil
}

func (c *Cache) reportReceiveFailure(w io.Writer, err error) error {
	refusal := receiveRefusal(err)
	if refusal == nil {
		return err
	}
	if c.config.Log != nil {
		c.config.Log(refusal.Reason(), err.Error())
	}
	feedback := refusal.Reason() + ". " + refusal.Remedy()
	return &reportedError{errors.Join(err, writeFeedback(w, []string{feedback}))}
}

func (c *Cache) writeReceiveNotices(w io.Writer, lines []string) {
	if err := writeFeedback(w, lines); err != nil && c.config.Log != nil {
		c.config.Log(reasonReceiveRestored, err.Error())
	}
}
