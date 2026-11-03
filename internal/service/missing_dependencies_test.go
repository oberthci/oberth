package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestMissingDependenciesAreUnavailable(t *testing.T) {
	s := &API{}
	for _, tc := range []struct{ name, args string }{
		{"issue_get", `{"id":1}`}, {"issue_create", `{"title":"work","body":"body"}`},
		{"issue_update", `{"id":1,"title":"work","body":"body"}`}, {"issue_append", `{"id":1,"text":"note"}`},
		{"issue_delete", `{"id":1}`}, {"promote_status", `{"id":"aaaaaaaa"}`},
	} {
		_, err := s.CallTool(t.Context(), api.Actor{Identity: "reader", Admin: true}, tc.name, json.RawMessage(tc.args))
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
