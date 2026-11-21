package sourcecontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestCanonicalJSONProperties(t *testing.T) {
	cases := []struct{ input, want string }{
		{` { "z": [3, 2, 1], "a": {"y":2,"x":1}} `, `{"a":{"x":1,"y":2},"z":[3,2,1]}`},
		{`{"n":9007199254740993,"s":"<é>"}`, `{"n":9007199254740993,"s":"<é>"}`},
		{`null`, `null`}, {`[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := CanonicalJSON(json.RawMessage(tc.input))
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %s, %v; want %s", got, err, tc.want)
			}
			again, err := CanonicalJSON(json.RawMessage(got))
			if err != nil || !bytes.Equal(got, again) {
				t.Fatal("canonical encoding was not idempotent")
			}
		})
	}
}

func TestCanonicalJSONRefusesMalformedSnapshots(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{`} {
		t.Run(raw, func(t *testing.T) {
			_, err := CanonicalJSON(json.RawMessage(raw))
			var target *json.SyntaxError
			if !errors.As(err, &target) {
				t.Fatalf("missing typed canonical refusal: %v", err)
			}
		})
	}
}

func TestCanonicalJSONPreservesUnsupportedValueCause(t *testing.T) {
	_, err := CanonicalJSON(make(chan int))
	var target *json.UnsupportedTypeError
	if !errors.As(err, &target) {
		t.Fatalf("cause lost: %v", err)
	}
}
