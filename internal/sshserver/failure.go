package sshserver

import (
	"errors"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

func gitFailure(err error) string {
	var reported interface{ Reported() bool }
	if errors.As(err, &reported) && reported.Reported() {
		return ""
	}
	var feedback interface{ GitFeedback() string }
	if errors.As(err, &feedback) {
		return feedback.GitFeedback()
	}
	var unavailable *gitport.UnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Reason() + " " + unavailable.Remedy()
	}
	var corrupt *gitcache.CorruptReceiveError
	if errors.As(err, &corrupt) {
		return corrupt.Error()
	}
	var author *gitport.AuthorRefusalError
	if errors.As(err, &author) {
		return author.Error()
	}
	var public *publicerr.Error
	if errors.As(err, &public) {
		return publicerr.Classify(err).Error()
	}
	var pending interface {
		Reason() string
		Remedy() string
	}
	if errors.As(err, &pending) {
		return pending.Reason() + ". " + pending.Remedy()
	}
	return reasonFailed + "; " + remedyRetry
}

type failureError struct{ reason string }

func (e *failureError) Reason() string { return e.reason }
func (e *failureError) Remedy() string {
	switch e.reason {
	case reasonConfig:
		return remedyConfig
	case reasonCommand:
		return remedyCommand
	}
	return remedyRetry
}
func (e *failureError) Error() string { return e.Reason() + ". " + e.Remedy() }
func failure(reason string) error     { return &failureError{reason: reason} }
