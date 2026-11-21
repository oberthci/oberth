package stateport

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (a Action) CheckIssueDeletion(v model.Issue) error {
	if a.Caller.Identity == "" || a.Caller.Admin || v.Imported == nil && v.Author == a.Caller.Identity {
		return nil
	}
	return publicerr.New(publicerr.Forbidden, "Only the creator or an administrator can delete this issue.", "Close the issue instead, or ask an administrator to delete it.", nil)
}
