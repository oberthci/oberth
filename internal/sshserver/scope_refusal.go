package sshserver

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/publicerr"
)

func (s *Server) refuseScope(ctx context.Context, identity Identity, command Command) error {
	cause := publicerr.New(publicerr.Forbidden, reasonScope+".", remedyScope, nil)
	if s.config.Refused == nil {
		return cause
	}
	return errors.Join(cause, s.config.Refused(ctx, Session{Identity: identity, Command: command}, cause))
}
