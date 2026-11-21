package strictjson

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

type request struct {
	Limit            int       `json:"limit"`
	Repo             string    `json:"repo"`
	ExpectedRevision *int64    `json:"expected_revision,omitempty"`
	At               time.Time `json:"at"`
	Items            []struct {
		Flag bool `json:"flag"`
	} `json:"items"`
	Meta map[string]any `json:"meta"`
}

func TestDecodeRefusalsAreActionablePublicErrors(t *testing.T) {
	cases := []struct{ name, input, reason, remedy string }{
		{"unknown field", `{"repos":[]}`, `unknown field "repos"`, "accepted fields: at, expected_revision, items, limit, meta, repo"},
		{"wrong type", `{"limit":"private"}`, `field "limit": expected an integer, got string`, "expected field type"},
		{"fraction", `{"limit":1.5}`, `field "limit": expected an integer, got number`, "range"},
		{"overflow", `{"limit":9999999999999999999999999}`, `field "limit": expected an integer`, "range"},
		{"nested type", `{"items":[{"flag":1}]}`, `field "items[0].flag": expected a boolean, got number`, "expected field type"},
		{"timestamp", `{"at":"private"}`, `field "at": expected an RFC 3339 timestamp`, "RFC 3339"},
		{"syntax", `{"limit":?}`, "Byte 10", "correct the JSON syntax"},
		{"incomplete", `{"limit":`, "ended before it was complete", "complete the JSON object"},
		{"trailing value", `{} {}`, "unexpected data after", "exactly one"},
		{"trailing garbage", `{} private`, "unexpected data after", "exactly one"},
		{"null root", `null`, "expected a JSON object", "non-null object"},
		{"array root", `[]`, "expected a JSON object", "non-null object"},
		{"empty input", ``, "expected a JSON object", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := request{Repo: "unchanged"}
			err := DecodeStrictObject([]byte(tc.input), &value, 4096)
			assertRefusal(t, err, tc.reason, tc.remedy)
			if value.Repo != "unchanged" || value.Limit != 0 {
				t.Fatal("refusal changed destination")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("request value leaked")
			}
		})
	}
}
func assertRefusal(t *testing.T, err error, reason, remedy string) {
	t.Helper()
	var public *publicerr.Error
	if !errors.As(err, &public) {
		t.Fatalf("no public error: %v", err)
	}
	if err := publicerr.CheckMessage(public.Reason(), 80); err != nil {
		t.Fatal(err, public.Reason())
	}
	if err := publicerr.CheckMessage(public.Remedy(), 120); err != nil {
		t.Fatal(err, public.Remedy())
	}
	if public.Code() != publicerr.InvalidInput || !strings.Contains(strings.ToLower(public.Error()), strings.ToLower(reason)) || !strings.Contains(strings.ToLower(public.Error()), strings.ToLower(remedy)) {
		t.Fatalf("unexpected diagnostic: %v", public)
	}
	if got := publicerr.Classify(err); got.Code() != publicerr.InvalidInput {
		t.Fatal("caller needs a conversion")
	}
}
func TestBoundsAndDestinationIdentifyTheirRefusal(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		dst    any
		max    int
		reason string
	}{
		{"size", []byte(`{"limit":1}`), &request{}, 2, "2-byte limit"},
		{"zero bound", []byte(`{}`), &request{}, 0, "invalid JSON size limit"},
		{"negative bound", []byte(`{}`), &request{}, -1, "invalid JSON size limit"},
		{"nil destination", []byte(`{}`), nil, 4096, "invalid JSON destination"},
		{"nil pointer", []byte(`{}`), (*request)(nil), 4096, "invalid JSON destination"},
		{"nonpointer", []byte(`{}`), request{}, 4096, "invalid JSON destination"},
		{"scalar destination", []byte(`{}`), new(int), 4096, "invalid JSON destination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertRefusal(t, DecodeStrictObject(tc.data, tc.dst, tc.max), tc.reason, "") })
	}
}
func TestStandardSemanticsAndReplacement(t *testing.T) {
	for _, input := range []string{`{}`, `{"expected_revision":null}`} {
		value := request{Repo: "clear"}
		if err := DecodeStrictObject([]byte(input), &value, len(input)); err != nil {
			t.Fatal(err)
		}
		if value.ExpectedRevision != nil || value.Repo != "" {
			t.Fatal("absence not preserved")
		}
	}
	var value request
	if err := DecodeStrictObject([]byte(`{"LIMIT":1,"limit":2,"meta":{"MiXeD":null}}`), &value, 4096); err != nil {
		t.Fatal(err)
	}
	if value.Limit != 2 {
		t.Fatal("standard duplicate/case behavior changed")
	}
	if _, ok := value.Meta["MiXeD"]; !ok {
		t.Fatal("extension key changed")
	}
}

type opaque struct{ received string }

func (v *opaque) UnmarshalJSON(data []byte) error { v.received = string(data); return nil }
func TestCustomUnmarshalerOwnsItsSchema(t *testing.T) {
	var value struct {
		Value opaque `json:"value"`
	}
	if err := DecodeStrictObject([]byte(`{"value":{"custom":true}}`), &value, 4096); err != nil {
		t.Fatal(err)
	}
	if value.Value.received != `{"custom":true}` {
		t.Fatal("custom decoder bypassed")
	}
}
func TestRoundTrip(t *testing.T) {
	for i := -10; i <= 10; i++ {
		original := request{Limit: i, Repo: "origin/org/repo", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var value request
		if err = DecodeStrictObject(data, &value, len(data)); err != nil {
			t.Fatal(err)
		}
		if value.Limit != i || value.Repo != original.Repo || !value.At.Equal(original.At) {
			t.Fatal("round trip changed value")
		}
	}
}

func TestMapKeyErrorNamesTheFieldAndExpectedType(t *testing.T) {
	var value struct {
		Counters map[int]string `json:"counters"`
	}
	err := DecodeStrictObject([]byte(`{"counters":{"private":"value"}}`), &value, 4096)
	assertRefusal(t, err, `field "counters": expected an integer, got a non-numeric object key`, "expected field type")
	if strings.Contains(err.Error(), "private") {
		t.Fatal("object key value leaked")
	}
}

type refusedValue struct{}

func (*refusedValue) UnmarshalJSON([]byte) error { return errors.New("private custom refusal") }
func TestNestedFailuresHaveFullPathsAndAcceptedNames(t *testing.T) {
	var value struct {
		Items []struct {
			When   time.Time    `json:"when"`
			Custom refusedValue `json:"custom"`
		} `json:"items"`
	}
	cases := []struct{ input, reason, remedy string }{
		{`{"items":[{"other":true}]}`, `unknown field "items[0].other"`, "accepted fields: custom, when"},
		{`{"items":[{"when":"private"}]}`, `field "items[0].when": expected an RFC 3339 timestamp`, "RFC 3339"},
		{`{"items":[{"custom":{}}]}`, `field "items[0].custom" could not be decoded`, "declared field format"},
	}
	for _, tc := range cases {
		err := DecodeStrictObject([]byte(tc.input), &value, 4096)
		assertRefusal(t, err, tc.reason, tc.remedy)
		if strings.Contains(err.Error(), "private") {
			t.Fatal("private value leaked")
		}
	}
}

func TestLongFieldNamesStayInDetail(t *testing.T) {
	key := strings.Repeat("field", 100)
	var value request
	err := DecodeStrictObject([]byte(`{"`+key+`":true}`), &value, 4096)
	assertRefusal(t, err, key, "Remove the unknown field")
	var e *publicerr.Error
	if !errors.As(err, &e) || !strings.Contains(e.Detail, key) {
		t.Fatal("full field name lost", err)
	}
}
