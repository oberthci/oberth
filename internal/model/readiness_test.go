package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadinessPublicCodesDoNotExposeCauses(t *testing.T) {
	cause := errors.New("private dependency details")
	for _, code := range []string{"no_upstream", "database_error", "ssh_identity", "startup_recovery", "private dependency details"} {
		err := fmt.Errorf("wrapped: %w", ReadinessFailure(code, cause))
		if !errors.Is(err, cause) {
			t.Fatal("lost internal error identity")
		}
		response := Readiness(err)
		raw, _ := json.Marshal(response)
		if !response.Valid() || strings.Contains(string(raw), cause.Error()) {
			t.Fatal("private readiness cause exposed")
		}
	}
	if response := Readiness(cause); response.Code != "unavailable" || !response.Valid() {
		t.Fatal("untyped errors must fail closed")
	}
	if response := Readiness(nil); response.Code != "ready" || !response.Valid() {
		t.Fatal("healthy state lost")
	}
}
