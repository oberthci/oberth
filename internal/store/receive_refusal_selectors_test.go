package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReviewValidEscapedRefBatchMustBeAudited(t *testing.T) {
	s := testStore(t)
	v := ReceiveRefusal{Code: "grant", File: ".oberth/build.yaml", Step: "build"}
	for i := range 256 {
		ref := "refs/heads/" + fmt.Sprintf("%03d", i) + strings.Repeat("&", 252)
		if err := sourcecontract.ValidatePublicRef(ref); err != nil {
			t.Fatal(err)
		}
		v.Updates = append(v.Updates, ReceiveRefusalRef{Ref: ref, Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)})
	}
	raw, err := json.Marshal(v)
	requireOK(t, err)
	if len(raw) <= maxReceiveRefusalBytes {
		t.Fatal("fixture does not exceed the receipt bound", len(raw))
	}
	err = s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", v)
	rows, _, readErr := s.Audit(t.Context(), 0, 200)
	if err != nil || readErr != nil || len(rows) != 1 {
		t.Fatalf("valid 256-ref refusal lost: encoded_bytes=%d, records=%d, write_error=%v, read_error=%v", len(raw), len(rows), err, readErr)
	}
	checkEscapedRefusalPrefix(t, rows[0].Details, v)
	for range 10 {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", v))
	}
	summary, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused.more", Limit: 20})
	requireOK(t, err)
	if len(summary) != 1 || summary[0].Details != `{"more_refusals":3}` {
		t.Fatal("escaped batch bypassed excess accounting", summary)
	}
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
}

func checkEscapedRefusalPrefix(t *testing.T, raw string, v ReceiveRefusal) {
	t.Helper()
	var detail ReceiveRefusal
	requireOK(t, json.Unmarshal([]byte(raw), &detail))
	if len(raw) > maxReceiveRefusalBytes || detail.OmittedUpdates != 240 || detail.SelectorsOmitted ||
		detail.File != v.File || detail.Step != v.Step || !reflect.DeepEqual(detail.Updates, v.Updates[:16]) {
		t.Fatal("escaped batch lost its bounded prefix or omitted count", detail)
	}
}

func TestReceiveRefusalOversizedSelectorsKeepDenial(t *testing.T) {
	s := testStore(t)
	for _, v := range []ReceiveRefusal{
		{Code: "grant", File: strings.Repeat("&", maxReceiveRefusalBytes)},
		{Code: "grant", Step: strings.Repeat("&", maxReceiveRefusalBytes)},
		{Code: "grant", Updates: []ReceiveRefusalRef{{Ref: strings.Repeat("&", maxReceiveRefusalBytes)}}},
	} {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", v))
		rows, _, err := s.Audit(t.Context(), 0, 200)
		requireOK(t, err)
		row := rows[len(rows)-1]
		var detail ReceiveRefusal
		requireOK(t, json.Unmarshal([]byte(row.Details), &detail))
		want := ReceiveRefusal{Code: "grant", OmittedUpdates: len(v.Updates), SelectorsOmitted: true}
		if len(row.Details) > maxReceiveRefusalBytes || !reflect.DeepEqual(detail, want) {
			t.Fatal("oversized selectors suppressed or overflowed the denial", detail)
		}
	}
}
