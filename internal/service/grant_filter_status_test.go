package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestGrantStatusFiltersRemainDistinct(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	db := testAccessStore(t, &now)
	up, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "origin", Kind: "ssh", BaseURL: "ssh://git@forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "oberth", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for _, status := range []string{"active", "revoked", "expired", "consumed"} {
		expiry := now.Add(24 * time.Hour)
		if status == "expired" {
			expiry = now.Add(time.Minute)
		}
		g, err := db.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: repo.ID, SourceSHA: strings.Repeat("a", 40), TargetRef: "main", Issuer: "admin", IssuerFingerprint: "key", ExpiresAt: expiry})
		if err != nil {
			t.Fatal(err)
		}
		expected[status] = g.ID
		if status == "revoked" {
			if _, err = db.RevokePromotionGrant(t.Context(), g.ID, "admin"); err != nil {
				t.Fatal(err)
			}
		}
		if status == "consumed" {
			if err = storetest.ConsumePromotionGrant(db, t.Context(), g.ID, "promotion", "admin"); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = now.Add(time.Hour)
	s, err := NewAPI(APIConfig{Runs: db, PromotionGrants: db})
	if err != nil {
		t.Fatal(err)
	}
	for status, id := range expected {
		rest, err := s.Grants(t.Context(), api.Actor{Identity: "reader"}, api.GrantFilter{Status: status})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(rest)
		var rows []map[string]any
		if err = json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["id"] != id {
			t.Errorf("REST %s: %s", status, raw)
		}
		value, err := s.CallTool(t.Context(), api.Actor{Identity: "reader"}, "promote_grant_list", json.RawMessage(`{"status":"`+status+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		rows = value.(api.PromotionGrantListResponse).Grants
		if len(rows) != 1 || rows[0]["id"] != id {
			t.Errorf("MCP %s: %+v", status, rows)
		}
	}
}
