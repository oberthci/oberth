package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func TestToolArgumentErrorsGiveAcceptedNames(t *testing.T) {
	var args struct {
		ID      string `json:"id"`
		Summary bool   `json:"summary"`
	}
	err := decodeTool(json.RawMessage(`{"bogus":true}`), &args)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), `unknown field "bogus"; accepted arguments: id, summary`) {
		t.Fatal(err)
	}
	var empty struct{}
	if err := decodeTool(json.RawMessage(`{"bogus":true}`), &empty); !strings.Contains(err.Error(), "accepted arguments: none") {
		t.Fatal(err)
	}
}

func TestToolNotFoundIncludesOnlyCallerSelector(t *testing.T) {
	for _, raw := range []string{`{"id":"missing-run"}`, `{"repo":"missing-repo"}`, `{"id":123}`} {
		err := toolSelectorError(json.RawMessage(raw), store.ErrNotFound)
		if !errors.Is(err, store.ErrNotFound) || err.Error() == store.ErrNotFound.Error() {
			t.Fatal(err)
		}
		if twice := toolSelectorError(json.RawMessage(raw), err); twice.Error() != err.Error() {
			t.Fatal(twice)
		}
	}
}

func TestWaitRequiresIDOrSHA(t *testing.T) {
	service := &API{}
	_, err := service.PrepareLongPoll(t.Context(), api.Actor{Identity: "tester"}, "wait", json.RawMessage(`{}`))
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "id, or sha, is required") {
		t.Fatal(err)
	}
}
