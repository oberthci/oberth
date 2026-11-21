package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestEveryReferenceValueCloneIsIndependent(t *testing.T) {
	values := []any{AgentEvent{}, Publication{}, Promotion{}, CIRequest{}, IssuePatch{}, Invitation{}, InvitationFile{}, InviteCertificate{}, EventPage{}, Issue{}}
	for _, value := range values {
		t.Run(reflect.TypeOf(value).Name(), func(t *testing.T) {
			typ := reflect.TypeOf(value)
			original := populated(typ)
			expected := populated(typ)
			method := original.MethodByName("Clone")
			if !method.IsValid() {
				t.Fatal("missing Clone")
			}
			cloned := method.Call(nil)[0]
			if !reflect.DeepEqual(cloned.Interface(), expected.Interface()) {
				t.Fatal("clone changes value")
			}
			mutateReferences(original)
			if !reflect.DeepEqual(cloned.Interface(), expected.Interface()) {
				t.Fatal("clone shares mutable storage")
			}
			zero := reflect.Zero(typ)
			zeroClone := zero.MethodByName("Clone").Call(nil)[0]
			if !reflect.DeepEqual(zero.Interface(), zeroClone.Interface()) {
				t.Fatal("zero clone changes absence")
			}
		})
	}
}
func populated(typ reflect.Type) reflect.Value {
	value := reflect.New(typ).Elem()
	if typ == reflect.TypeFor[time.Time]() {
		value.Set(reflect.ValueOf(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
		return value
	}
	switch typ.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(typ.Elem()))
		value.Elem().Set(populated(typ.Elem()))
	case reflect.Slice:
		value.Set(reflect.MakeSlice(typ, 1, 1))
		value.Index(0).Set(populated(typ.Elem()))
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Field(i).CanSet() {
				value.Field(i).Set(populated(typ.Field(i).Type))
			}
		}
	case reflect.String:
		value.SetString("value")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint8:
		value.SetUint('x')
	}
	return value
}
func mutateReferences(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		value.Elem().Set(reflect.Zero(value.Type().Elem()))
	case reflect.Slice:
		value.Index(0).Set(reflect.Zero(value.Type().Elem()))
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Field(i).CanSet() {
				mutateReferences(value.Field(i))
			}
		}
	}
}
func TestVocabulariesGrantNothingAtZero(t *testing.T) {
	vocabulary(t, []RunTrigger{TriggerBranch, TriggerPromotion, TriggerSchedule, TriggerTag})
	vocabulary(t, []PublicationStatus{PublicationPending, PublicationDelivered, PublicationFailed, PublicationAbandoned})
	vocabulary(t, []PromotionStatus{PromotionPending, PromotionPassed, PromotionFailed, PromotionInterrupted})
	vocabulary(t, []EventKind{EventIssueUpdated, EventRunFinished, EventRunHeld, EventRunLockWait, EventPromotionFinished, EventReleasePublished, EventPublicationFailed, EventRunUpdated})
	vocabulary(t, []IssueKind{IssueManual, IssueCI})
	vocabulary(t, []IssueState{IssueOpen, IssueClosed})
	vocabulary(t, []IssueWorkStatus{IssueTodo, IssueActive, IssueWaiting, IssueReview})
}
func vocabulary[T interface {
	~string
	Valid() bool
}](t *testing.T, values []T) {
	t.Helper()
	seen := map[T]bool{}
	for _, value := range values {
		if !value.Valid() || seen[value] {
			t.Fatalf("invalid vocabulary %v", value)
		}
		seen[value] = true
	}
	for _, invalid := range []T{"", "other", " ", "PENDING"} {
		if invalid.Valid() {
			t.Fatalf("invalid value accepted: %q", invalid)
		}
	}
}
func TestEventProducerBounds(t *testing.T) {
	if MaxEventSummaryRunes != 512 || MaxEventDataBytes != 8<<10 || MaxEventPage != 200 || MaxEventFilters != 32 {
		t.Fatal("event producer bounds changed")
	}
}
func TestIssueRevisionOmissionAndPositiveCAS(t *testing.T) {
	positive := int64(1)
	zero := int64(0)
	negative := int64(-1)
	for _, revision := range []*int64{nil, &positive} {
		if err := (IssuePatch{ID: 1, ExpectedRevision: revision}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, patch := range []IssuePatch{{}, {ID: -1}, {ID: 1, ExpectedRevision: &zero}, {ID: 1, ExpectedRevision: &negative}} {
		var typed *publicerr.Error
		if !errors.As(patch.Validate(), &typed) {
			t.Fatal("invalid revision accepted")
		}
	}
}
func TestEmptyCollectionsAndNullIdentitySurviveClone(t *testing.T) {
	page := EventPage{Events: []AgentEvent{}}
	raw, err := json.Marshal(page.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Fatal("empty events are not an array")
	}

}

func TestOptionalIssuePatchAndFlatInvitationFile(t *testing.T) {
	raw, err := json.Marshal(IssuePatch{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"id":1}` {
		t.Fatal("omitted patch selectors became null")
	}
	raw, err = json.Marshal(InvitationFile{Invitation: Invitation{ID: "invite"}, Secret: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"id":"invite"`) || strings.Contains(string(raw), `"binding"`) {
		t.Fatal("invitation binding is not flat")
	}
}

func TestIssuePatchReportsEveryOffender(t *testing.T) {
	revision := int64(0)
	status := IssueWorkStatus("unknown")
	err := (IssuePatch{ExpectedRevision: &revision, WorkStatus: &status}).Validate()
	var public *publicerr.Error
	if !errors.As(err, &public) {
		t.Fatal("missing public refusal")
	}
	for _, reason := range []string{"invalid issue ID", "invalid expected revision", "invalid work_status"} {
		if !strings.Contains(public.Error(), reason) {
			t.Fatal("offender lost", reason)
		}
	}
}

func TestEveryIssuePatchDiagnosticFits(t *testing.T) {
	revision := int64(0)
	status := IssueWorkStatus("bad")
	for _, patch := range []IssuePatch{{}, {ID: 1, ExpectedRevision: &revision}, {ID: 1, WorkStatus: &status}, {ExpectedRevision: &revision, WorkStatus: &status}} {
		e := publicerr.Classify(patch.Validate())
		for text, limit := range map[string]int{e.Reason(): 80, e.Remedy(): 120} {
			if err := publicerr.CheckMessage(text, limit); err != nil {
				t.Fatal(err, text)
			}
		}
	}
}
