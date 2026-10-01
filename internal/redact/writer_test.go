package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestWriterMasksAcrossWriteBoundaries(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("correct-horse-battery-staple")})
	for _, fragment := range []string{"before correct-", "horse-battery", "-staple after"} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "before *** after" {
		t.Fatalf("output = %q", got)
	}
}

func TestWriterMasksLongestValueFirstAndEveryNonemptyValue(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{
		[]byte("token"), []byte("token-long"), []byte("abc"), []byte("first-line\r\nsecond-line\n"), nil,
	})
	_, _ = writer.Write([]byte("token-long token abc first-line second-line first-line\r\nsecond-line"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "*** *** *** *** *** ***" {
		t.Fatalf("output = %q", got)
	}
}

func TestClosedWriterRejectsWritesWithoutLeakingInput(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("secret")})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q", output.String())
	}
}

func TestWriterMasksBase64EncodedSecret(t *testing.T) {
	secret := []byte("supersecretvalue1234567890abcdef")
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{secret})
	encoded := base64.StdEncoding.EncodeToString(secret)
	if _, err := writer.Write([]byte("auth: " + encoded + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "***") {
		t.Fatalf("base64-encoded secret was not masked: %q", got)
	}
	if strings.Contains(got, encoded) {
		t.Fatalf("base64-encoded secret passed through unmasked: %q", got)
	}
}

func TestWriterMasksHexEncodedSecret(t *testing.T) {
	secret := []byte("hexsecretvalue1234567890")
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{secret})
	lower := hex.EncodeToString(secret)
	upper := strings.ToUpper(lower)
	if _, err := writer.Write([]byte("lower: " + lower + "\nupper: " + upper + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, lower) {
		t.Fatalf("hex-encoded secret (lowercase) was not masked: %q", got)
	}
	if strings.Contains(got, upper) {
		t.Fatalf("hex-encoded secret (uppercase) was not masked: %q", got)
	}
}

func TestWriterMasksBase64WithMisalignedPrefix(t *testing.T) {
	// THE decisive test: "_json_key:" is 10 bytes (10%3=1), so the secret
	// starts at phase 1 within base64 encoding groups. A naive base64(secret)
	// pattern will NOT match; only phase-aware registration catches this.
	secret := []byte(`{"type":"service_account","project_id":"test-project","private_key_id":"key123456789"}`)
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{secret})

	prefix := "_json_key:"
	payload := append([]byte(prefix), secret...)
	encoded := base64.StdEncoding.EncodeToString(payload)
	if _, err := writer.Write([]byte(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	if !strings.Contains(got, "***") {
		t.Fatalf("expected masking in base64-encoded payload, got %q", got)
	}

	// Leading chars that encode only the prefix may survive; the secret-
	// derived region must be masked. The prefix is 10 bytes, so the pure-
	// secret encoding starts at char 14 (group 3, char position 2).
	// No 16-char contiguous run from that point onward should survive.
	const pureSecretStart = 14
	for i := pureSecretStart; i+16 <= len(encoded); i++ {
		chunk := encoded[i : i+16]
		if strings.Contains(got, chunk) {
			t.Fatalf("16-char run of secret encoding survived at offset %d: %q\nfull output: %q", i, chunk, got)
		}
	}
}

func TestWriterMasksPEMLineEncodedAsBase64(t *testing.T) {
	pemSecret := []byte("-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn\n" +
		"secondlineofkeydatahere1234567890\n" +
		"-----END RSA PRIVATE KEY-----\n")
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{pemSecret})

	// A single PEM body line, base64-encoded, as if someone ran
	// echo "$LINE" | base64
	pemLine := []byte("MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn")
	lineEncoded := base64.StdEncoding.EncodeToString(pemLine)
	if _, err := writer.Write([]byte("leaked: " + lineEncoded + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	if strings.Contains(got, lineEncoded) {
		t.Fatalf("base64-encoded PEM line passed through unmasked: %q", got)
	}
}

func TestWriterMergesOverlappingPatterns(t *testing.T) {
	// "AB" starts at index 1, "BCDE" starts at index 2. Without merging,
	// redacting "AB" alone leaks "CDE" from the longer overlapping pattern.
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("AB"), []byte("BCDE")})
	if _, err := writer.Write([]byte("XABCDEY")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if got != "X***Y" {
		t.Fatalf("expected X***Y, got %q", got)
	}
}

func TestWriterMergesMultipleOverlapping(t *testing.T) {
	// Three overlapping patterns forming one contiguous region.
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("AB"), []byte("BC"), []byte("CD")})
	if _, err := writer.Write([]byte("0ABCD1")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if got != "0***1" {
		t.Fatalf("expected 0***1, got %q", got)
	}
}

func TestWriterChainedOverlapBridge(t *testing.T) {
	// Bridge case: A overlaps B, B overlaps C, but A does not overlap C.
	// Depending on the sort order (all three have different lengths), the
	// initial merge pass may process C before B, missing C's extension.
	// The fixed-point loop catches it.
	//
	// Lengths: "AABBCC"=6, "DDEEF"=5, "CCDD"=4 — sorted longest-first,
	// the processing order is deterministically AABBCC, DDEEF, CCDD.
	// DDEEF starts at position 7 (after AABBCC ends at 7), so it is
	// skipped by the initial pass. CCDD starts at position 5, inside
	// AABBCC, and extends bestEnd to 9. Only the fixed-point loop then
	// picks up DDEEF at 7 < 9 and extends to 12.
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{
		[]byte("AABBCC"), []byte("CCDD"), []byte("DDEEF"),
	})
	if _, err := writer.Write([]byte("0AABBCCDDEEF1")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if got != "0***1" {
		t.Fatalf("chained-overlap bridge: expected 0***1, got %q", got)
	}
}

func TestWriterRedactsShortValues(t *testing.T) {
	// Values shorter than 8 bytes must still be redacted when passed as
	// exact secrets. The minFragmentLen threshold applies only to
	// line-split fragments of multiline values, not to full values.
	for _, tc := range []struct {
		name   string
		secret string
		input  string
		want   string
	}{
		{"5-byte", "abc12", "token=abc12 done", "token=*** done"},
		{"3-byte", "key", "the key is key!", "the *** is ***!"},
		{"1-byte", "X", "AXB", "A***B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			writer := NewWriter(&output, [][]byte{[]byte(tc.secret)})
			if _, err := writer.Write([]byte(tc.input)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriterShortLineFragmentNotRegistered(t *testing.T) {
	// A multiline secret where one line is very short (3 bytes "v=1").
	// The full value must be redacted, but the short line fragment alone
	// should NOT cause false-positive redaction of unrelated occurrences.
	multiline := []byte("long-enough-key=abc\nv=1\n")
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{multiline})

	// The full multiline value must be redacted.
	if _, err := writer.Write(multiline); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "***") {
		t.Fatalf("full multiline value was not redacted: %q", got)
	}

	// But the short fragment "v=1" alone in unrelated text should pass through.
	var output2 bytes.Buffer
	writer2 := NewWriter(&output2, [][]byte{multiline})
	if _, err := writer2.Write([]byte("config: v=1 is fine")); err != nil {
		t.Fatal(err)
	}
	if err := writer2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output2.String(); got != "config: v=1 is fine" {
		t.Fatalf("short line fragment caused false-positive redaction: %q", got)
	}
}
