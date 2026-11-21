package stateport_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestImportedAuthorDoesNotGrantIssueDeletion(t *testing.T) {
	action := stateport.Action{Actor: "worker", Caller: actor.Actor{Identity: "worker"}}
	issue := model.Issue{Author: "worker", Imported: &model.IssueImportSource{}}
	if err := action.CheckIssueDeletion(issue); err == nil {
		t.Fatal("source attribution became local deletion authority")
	}
	action.Caller.Admin = true
	if err := action.CheckIssueDeletion(issue); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedIssueDeletionNamesTheAdministratorCommand(t *testing.T) {
	action := stateport.Action{Actor: "worker", Caller: actor.Actor{Identity: "worker"}}
	err := action.CheckIssueDeletion(model.Issue{ID: 42, Author: "someone"})
	var p *publicerr.Error
	if !errors.As(err, &p) || p.Code() != publicerr.Forbidden || p.Command() != "oberth issue delete 42" {
		t.Fatal("refusal lacks the exact deletion command", err)
	}
}
