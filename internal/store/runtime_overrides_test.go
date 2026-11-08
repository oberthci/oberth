package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestReleaseWIFOverrideGrantRevokeResetAudited(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	repo := "github/example/app"
	if got, err := s.ReleaseWIFOverrides(ctx); err != nil || len(got) != 0 {
		t.Fatalf("fresh overrides = %+v %v", got, err)
	}
	if _, err := s.ClearReleaseWIFOverride(ctx, "admin", model.ReleaseWIFOverrideSpec{Repo: repo, Reason: "nothing"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("reset without override: %v", err)
	}
	for _, bad := range []model.ReleaseWIFOverrideSpec{
		{Repo: repo, Grant: json.RawMessage(`{"templates":{"publish":"image-writer"}}`)},
		{Repo: repo, Grant: json.RawMessage(`{}`), Reason: "empty"},
		{Repo: repo, Grant: json.RawMessage(`[]`), Reason: "array"},
		{Repo: repo, Grant: json.RawMessage(`{"templates":{"publish":"image-writer"}}`), Reason: strings.Repeat("x", 513)},
	} {
		if _, err := s.SetReleaseWIFOverride(ctx, "admin", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid spec %+v accepted: %v", bad, err)
		}
	}
	granted, err := s.SetReleaseWIFOverride(ctx, "admin", model.ReleaseWIFOverrideSpec{Repo: repo, Grant: json.RawMessage(`{ "templates": {"publish": "image-writer"} }`), Reason: "absorb e2e"})
	if err != nil || granted.Revoked || string(granted.Grant) != `{"templates":{"publish":"image-writer"}}` || granted.Actor != "admin" {
		t.Fatalf("grant = %+v %v", granted, err)
	}
	revoked, err := s.SetReleaseWIFOverride(ctx, "admin2", model.ReleaseWIFOverrideSpec{Repo: repo, Revoked: true, Reason: "retired", PreviousEffective: json.RawMessage(`{"service_account_name":"app-release"}`)})
	if err != nil || !revoked.Revoked || revoked.Grant != nil || revoked.Actor != "admin2" {
		t.Fatalf("revoke = %+v %v", revoked, err)
	}
	if got, err := s.ReleaseWIFOverrides(ctx); err != nil || len(got) != 1 || !got[0].Revoked {
		t.Fatalf("overrides after revoke = %+v %v", got, err)
	}
	cleared, err := s.ClearReleaseWIFOverride(ctx, "admin", model.ReleaseWIFOverrideSpec{Repo: repo, Reason: "back to helm", PreviousEffective: json.RawMessage(`{"service_account_name":"app-release"}`), Effective: json.RawMessage(`{"service_account_name":"helm-release"}`)})
	if err != nil || !cleared.Revoked {
		t.Fatalf("reset = %+v %v", cleared, err)
	}
	if got, err := s.ReleaseWIFOverrides(ctx); err != nil || len(got) != 0 {
		t.Fatalf("overrides after reset = %+v %v", got, err)
	}
	rows, err := s.db.Query(`SELECT action,actor,details FROM audit_actions WHERE resource_type='release_wif' AND resource_id=? ORDER BY id`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actions []string
	for rows.Next() {
		var action, actor, details string
		if err := rows.Scan(&action, &actor, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if !strings.Contains(details, `"reason"`) || (action != "release_wif.grant" && !strings.Contains(details, `"previous"`)) {
			t.Fatalf("%s details lack reason/previous: %s", action, details)
		}
	}
	if strings.Join(actions, ",") != "release_wif.grant,release_wif.revoke,release_wif.reset" {
		t.Fatalf("audit actions = %v", actions)
	}
	// The effective entries record a replaced or removed Helm grant that no
	// override row holds; an absent entry is an explicit null.
	effective := map[string][2]string{
		"release_wif.grant":  {`null`, `null`},
		"release_wif.revoke": {`{"service_account_name":"app-release"}`, `null`},
		"release_wif.reset":  {`{"service_account_name":"app-release"}`, `{"service_account_name":"helm-release"}`},
	}
	for action, want := range effective {
		var details struct {
			PreviousEffective json.RawMessage `json:"previous_effective"`
			Effective         json.RawMessage `json:"effective"`
		}
		var body string
		if err := s.db.QueryRow(`SELECT details FROM audit_actions WHERE resource_type='release_wif' AND resource_id=? AND action=?`, repo, action).Scan(&body); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &details); err != nil {
			t.Fatal(err)
		}
		if string(details.PreviousEffective) != want[0] || string(details.Effective) != want[1] {
			t.Fatalf("%s effective entries = %s -> %s, want %s -> %s", action, details.PreviousEffective, details.Effective, want[0], want[1])
		}
	}
}

func TestAdmissionCapacityOverrideSetClearAudited(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	if got, err := s.AdmissionCapacityOverride(ctx); err != nil || got != nil {
		t.Fatalf("fresh override = %+v %v", got, err)
	}
	if _, err := s.SetAdmissionCapacityOverride(ctx, "admin", 0, "clear"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("clear without override: %v", err)
	}
	for _, jobs := range []int{-1, 65} {
		if _, err := s.SetAdmissionCapacityOverride(ctx, "admin", jobs, "bad"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("jobs %d accepted: %v", jobs, err)
		}
	}
	if _, err := s.SetAdmissionCapacityOverride(ctx, "admin", 4, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank reason accepted: %v", err)
	}
	set, err := s.SetAdmissionCapacityOverride(ctx, "admin", 4, "host overload")
	if err != nil || set == nil || set.MaxConcurrentJobs != 4 || set.Reason != "host overload" {
		t.Fatalf("set = %+v %v", set, err)
	}
	if set, err = s.SetAdmissionCapacityOverride(ctx, "admin", 6, "recovered"); err != nil || set.MaxConcurrentJobs != 6 {
		t.Fatalf("replace = %+v %v", set, err)
	}
	if cleared, err := s.SetAdmissionCapacityOverride(ctx, "admin", 0, "back to helm"); err != nil || cleared != nil {
		t.Fatalf("clear = %+v %v", cleared, err)
	}
	var count int
	var last string
	if err := s.db.QueryRow(`SELECT count(*),max(details) FROM audit_actions WHERE resource_type='admission' AND action LIKE 'admission.capacity.override%'`).Scan(&count, &last); err != nil || count != 3 {
		t.Fatalf("audit count = %d %v", count, err)
	}
	var details string
	if err := s.db.QueryRow(`SELECT details FROM audit_actions WHERE action='admission.capacity.override.clear'`).Scan(&details); err != nil || !strings.Contains(details, `"max_concurrent_jobs":6`) {
		t.Fatalf("clear audit lacks previous value: %s %v", details, err)
	}
}
