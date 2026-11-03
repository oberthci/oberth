package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func TestUplinkListFlagsOnlyActiveTokensWithoutExpiry(t *testing.T) {
	f := newScopeFixture(t)
	now := time.Now()
	f.service.uplinkLifecycle = &lifecycleStub{statuses: []store.UplinkStatus{{}, {Pending: true}, {RevokedAt: &now}, {ExpiresAt: &now}}}
	value, err := f.call(unscopedActor, "uplink_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Uplinks []struct {
			NoExpiry *bool `json:"no_expiry"`
		} `json:"uplinks"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	for index, uplink := range result.Uplinks {
		if uplink.NoExpiry == nil || *uplink.NoExpiry != (index == 0) {
			t.Errorf("uplink %d expiry flag = %v", index, uplink.NoExpiry)
		}
	}
}
