package stateport_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
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
