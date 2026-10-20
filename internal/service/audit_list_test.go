package service

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestAuditListIsAdminOnlyAndReturnsChainPositions(t *testing.T) {
	t.Parallel()
	fixture := newScopeFixture(t)

	for _, actor := range []api.Actor{scopedActor, scopedAdmin} {
		if _, err := fixture.call(actor, "audit_list", map[string]any{}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("scoped %s audit_list error = %v, want forbidden", actor.Identity, err)
		}
	}
	if _, err := fixture.call(api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}, "audit_list", map[string]any{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin audit_list error = %v, want forbidden", err)
	}

	value, err := fixture.call(unscopedActor, "audit_list", map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(AuditListResponse)
	if len(page.Entries) != 2 || page.HeadID <= 2 || page.NextBefore != page.Entries[1].ID {
		t.Fatalf("page = %+v", page)
	}
	if page.Entries[0].ID != page.HeadID {
		t.Fatalf("newest entry %d is not the head %d", page.Entries[0].ID, page.HeadID)
	}
	if !page.Entries[0].Verified || page.Entries[0].PreviousSHA256 == "" {
		t.Fatalf("entry = %+v", page.Entries[0])
	}
	older, err := fixture.call(unscopedActor, "audit_list", map[string]any{"before": page.NextBefore, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	next := older.(AuditListResponse).Entries[0]
	if next.ID != page.NextBefore-1 || next.SHA256 != page.Entries[1].PreviousSHA256 {
		t.Fatalf("older entry %+v does not precede %+v in the chain", next, page.Entries[1])
	}
	if _, err := hex.DecodeString(next.SHA256); err != nil {
		t.Fatal(err)
	}

	filtered, err := fixture.call(unscopedActor, "audit_list", map[string]any{"action": "repository.", "actor": "owner@tuxbox", "repo": "oberth"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range filtered.(AuditListResponse).Entries {
		if len(entry.Action) < 11 || entry.Action[:11] != "repository." {
			t.Fatalf("entry %+v escaped the action filter", entry)
		}
	}
	if _, err := fixture.call(unscopedActor, "audit_list", map[string]any{"since": "yesterday"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad since error = %v", err)
	}
}
