package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestCallToolMapsInvalidStoreInput(t *testing.T) {
	f := newControlFixture(t)
	_, err := f.api(t).CallTool(t.Context(), api.Actor{Identity: "reader"}, "issue_lock", json.RawMessage(`{"id":0}`))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid issue_lock = %v", err)
	}
}
