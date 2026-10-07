package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestUnquoteJSONBytesMatchesEncodingJSON: for every string encoding/json can
// produce, the byte decoder yields exactly the original bytes — PEM bodies
// with newlines, quotes, backslashes, HTML-escaped characters, and non-ASCII.
func TestUnquoteJSONBytesMatchesEncodingJSON(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"plain",
		"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----\n",
		"tab\there \"quoted\" back\\slash /slash",
		"control \x01\x1f chars and    ",
		"html <>& chars",
		"café 日本 \U0001F600",
		"\r\n\b\f",
	}
	for _, want := range cases {
		literal, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := unquoteJSONBytes(literal)
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Fatalf("%q: decoded %q", want, got)
		}
	}
}

// TestUnquoteJSONBytesEscapesAndErrors covers hand-written literals that
// encoding/json would not emit but a store could hold, and the malformed
// forms that must be rejected rather than replaced with U+FFFD.
func TestUnquoteJSONBytesEscapesAndErrors(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		`"😀"`:              "\U0001F600",
		`"éé"`:             "éé",
		`"a\/b"`:           "a/b",
		`  "spaced"  `:     "spaced",
		`"A\u0000"`:        "A\x00",
		`"\"\\\b\f\n\r\t"`: "\"\\\b\f\n\r\t",
	}
	for literal, want := range ok {
		got, err := unquoteJSONBytes([]byte(literal))
		if err != nil {
			t.Fatalf("%s: %v", literal, err)
		}
		if string(got) != want {
			t.Fatalf("%s: decoded %q want %q", literal, got, want)
		}
	}
	if got, err := unquoteJSONBytes([]byte("null")); err != nil || got != nil {
		t.Fatalf("null must decode to nil, nil: %v %v", got, err)
	}
	bad := []string{
		`"\ud83d"`,       // unpaired high surrogate
		`"\ud83dx"`,      // high surrogate not followed by an escape
		`"\ude00"`,       // unpaired low surrogate
		`"\ud83dA"`,      // high surrogate followed by a non-low escape
		`"\x41"`,         // invalid escape
		`"\u12"`,         // truncated \u
		`"\u12g4"`,       // non-hex digit
		`"trailing\"`,    // truncated escape at end
		"\"raw\x01ctl\"", // unescaped control character
		`123`,            // not a string
		`"unterminated`,  // missing closing quote
		`"`,              // single quote
		``,               // empty
	}
	for _, literal := range bad {
		if got, err := unquoteJSONBytes([]byte(literal)); err == nil {
			t.Fatalf("%q must be rejected, decoded %q", literal, got)
		}
	}
}

// TestDecodeIdentityBundleFieldsVersionAndClear decodes a realistic
// `bao read -format=json` response (with the warning lines bao prints around
// it), checks every field and the version, and proves clear() zero-fills
// every value the bundle handed out.
func TestDecodeIdentityBundleFieldsVersionAndClear(t *testing.T) {
	t.Parallel()
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----\n"
	payload := baoIdentityJSON(7, map[string]string{
		"tls.crt":      "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		"tls.key":      key,
		"ssh_host_key": "ssh \"host\" key\\with\tescapes",
	})
	// A null field is absent; a non-string field is an error (tested below).
	payload = strings.Replace(payload, `"ssh_host_key"`, `"empty":null,"ssh_host_key"`, 1)
	raw := []byte("WARNING! stray line before the object\n" + payload + "\ntrailing noise\n")

	bundle, err := decodeIdentityBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.version != 7 {
		t.Fatalf("version: got %d want 7", bundle.version)
	}
	if string(bundle.field("tls.key")) != key {
		t.Fatalf("tls.key: got %q", bundle.field("tls.key"))
	}
	if string(bundle.field("ssh_host_key")) != "ssh \"host\" key\\with\tescapes" {
		t.Fatalf("ssh_host_key: got %q", bundle.field("ssh_host_key"))
	}
	if bundle.field("empty") != nil {
		t.Fatalf("null field must be absent, got %q", bundle.field("empty"))
	}
	if bundle.field("missing") != nil || (*identityBundle)(nil).field("x") != nil {
		t.Fatal("absent fields and nil bundles must yield nil")
	}
	if len(bundle.data) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(bundle.data))
	}

	handed := [][]byte{bundle.field("tls.crt"), bundle.field("tls.key"), bundle.field("ssh_host_key")}
	bundle.clear()
	for i, value := range handed {
		if !allZero(value) {
			t.Fatalf("value %d was not zero-filled by clear(): %q", i, value)
		}
	}
	if len(bundle.data) != 0 {
		t.Fatal("clear() must empty the map")
	}
	bundle.clear() // idempotent
	(*identityBundle)(nil).clear()
}

// TestDecodeIdentityBundleRejectsNonStringAndGarbage: a non-string field or
// no JSON object at all is an error, and the error carries no output bytes.
func TestDecodeIdentityBundleRejectsNonStringAndGarbage(t *testing.T) {
	t.Parallel()
	secret := "SENTINEL-SECRET-VALUE"
	_, err := decodeIdentityBundle([]byte(`{"data":{"data":{"tls.key":"` + secret + `","n":42},"metadata":{"version":1}}}`))
	if err == nil {
		t.Fatal("non-string field must be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks field bytes: %v", err)
	}
	if _, err := decodeIdentityBundle([]byte("No value found " + secret)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("garbage must be rejected without echoing it: %v", err)
	}
	if _, err := decodeIdentityBundle([]byte(`{"data":{"data":"` + secret + `"}}`)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("wrong data shape must be rejected without echoing it: %v", err)
	}
}

// TestReadRawReturnsExactStdoutAndNilWhenAbsent: readRaw hands the caller the
// exec stdout untouched (so clear() on it clears the only copy) and maps the
// CLI's "No value found" to nil, nil like readData does.
func TestReadRawReturnsExactStdoutAndNilWhenAbsent(t *testing.T) {
	t.Parallel()
	payload := baoIdentityJSON(3, map[string]string{"password": "abc"})
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/rekor/rekor-db": {out: payload},
		"read -format=json " + defaultKVPrefix + "/data/identities/rekor/absent":   {out: "No value found at ...", err: errors.New("exit status 2")},
		"read -format=json " + defaultKVPrefix + "/data/identities/rekor/broken":   {out: "permission denied", err: errors.New("exit status 2")},
	}}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	raw, err := store.readRaw(context.Background(), "root", defaultKVPrefix+"/data/identities/rekor/rekor-db")
	if err != nil || string(raw) != payload {
		t.Fatalf("readRaw: %q %v", raw, err)
	}
	absent, err := store.readRaw(context.Background(), "root", defaultKVPrefix+"/data/identities/rekor/absent")
	if err != nil || absent != nil {
		t.Fatalf("absent path must be nil, nil: %q %v", absent, err)
	}
	if _, err := store.readRaw(context.Background(), "root", defaultKVPrefix+"/data/identities/rekor/broken"); err == nil {
		t.Fatal("non-absence failure must propagate")
	}
	// Every authenticated call's stdin (token line) was cleared after the exec.
	for _, call := range runner.calls {
		if !call.authenticated {
			continue
		}
		if !strings.HasPrefix(call.stdin, "root\n") {
			t.Fatalf("token line missing from stdin copy: %q", call.stdin)
		}
	}
}
