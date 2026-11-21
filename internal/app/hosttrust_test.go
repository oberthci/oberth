package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func hostKey(t *testing.T, b byte) ssh.PublicKey {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	key, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func scanner(keys ...ssh.PublicKey) fake.Scanner {
	n := 0
	return fake.Scanner{ScanFunc: func(context.Context, string, string) (ssh.PublicKey, error) {
		if n >= len(keys) {
			return nil, errors.New("unsupported algorithm")
		}
		key := keys[n]
		n++
		return key, nil
	}}
}

// Covers: REQ-C3-002
func TestReviewedEndpointTrustContract(t *testing.T) {
	t.Parallel()
	first, second := hostKey(t, 1), hostKey(t, 2)
	fp := ssh.FingerprintSHA256(first)
	result, err := app.TrustHost(context.Background(), scanner(first, second), app.TrustRequest{Address: "forge:22", Fingerprints: []string{fp}})
	if err != nil || len(result.Offered) != 2 || strings.Contains(string(result.Lines), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(second)))) {
		t.Fatal(result, err)
	}
	want := knownhosts.Line([]string{"forge:22"}, first) + "\n"
	if string(result.Lines) != want {
		t.Fatal(string(result.Lines))
	}
	for _, fingerprints := range [][]string{nil, {"SHA256:wrong"}, {fp[:len(fp)-1]}} {
		got, err := app.TrustHost(context.Background(), scanner(first), app.TrustRequest{Address: "forge:22", Fingerprints: fingerprints})
		expectRefusal(t, err, "trust")
		if len(got.Lines) != 0 || len(got.Offered) != 1 {
			t.Fatal("unreviewed key trusted", got)
		}
	}

	testRepin(t, first, second)
}
func testRepin(t *testing.T, first, second ssh.PublicKey) {
	t.Helper()
	fp := ssh.FingerprintSHA256(first)
	want := knownhosts.Line([]string{"forge:22"}, first) + "\n"
	unrelated := "# keep this exact comment\n" + knownhosts.Line([]string{"other:22"}, second) + " note\n"
	old := []byte(unrelated + want)
	result, err := app.Repin(context.Background(), scanner(second), app.TrustRequest{Address: "forge:22", Fingerprints: []string{ssh.FingerprintSHA256(second)}}, old)
	if err != nil || len(result.Replaced) != 1 || result.Replaced[0] != fp || !bytes.HasPrefix(result.Lines, []byte(unrelated)) {
		t.Fatal(result, err)
	}
	if bytes.Contains(result.Lines, []byte(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(first))))) {
		t.Fatal("old pin retained")
	}
}
func TestRepinRefusesAmbiguousOrMalformedLines(t *testing.T) {
	t.Parallel()
	key := hostKey(t, 1)
	for _, line := range []string{
		"invalid", knownhosts.Line([]string{"forge:22", "other:22"}, key), knownhosts.Line([]string{"*.example:22"}, key), knownhosts.Line([]string{knownhosts.HashHostname("forge")}, key),
	} {
		_, err := app.Repin(context.Background(), scanner(key), app.TrustRequest{Address: "forge:22", Fingerprints: []string{ssh.FingerprintSHA256(key)}}, []byte(line+"\n"))
		expectRefusal(t, err, "pins")
	}
}
func TestTrustCancellationAndNoKeys(t *testing.T) {
	t.Parallel()
	key := hostKey(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := fake.Scanner{ScanFunc: func(ctx context.Context, _ string, _ string) (ssh.PublicKey, error) { return nil, ctx.Err() }}
	_, err := app.TrustHost(ctx, s, app.TrustRequest{Address: "forge:22", Fingerprints: []string{ssh.FingerprintSHA256(key)}})
	expectRefusal(t, err, "trust")
	_, err = app.TrustHost(context.Background(), scanner(), app.TrustRequest{Address: "forge:22", Fingerprints: []string{ssh.FingerprintSHA256(key)}})
	expectRefusal(t, err, "trust")
	var refusal *app.Refusal
	if !errors.As(err, &refusal) || refusal.Detail != "unsupported algorithm" {
		t.Fatal("a scan without any key lost what failed, or repeated it", err)
	}
	_, err = app.TrustHost(context.Background(), scanner(key), app.TrustRequest{Address: "forge:22"})
	if !errors.As(err, &refusal) || refusal.Detail != "" {
		t.Fatal("an offered key carried scan failures as detail", err)
	}
}

func TestTrustRefusesAddressInjection(t *testing.T) {
	t.Parallel()
	key := hostKey(t, 1)
	for _, address := range []string{"forge", "forge:22\nother key", "forge:22,other", " :22", "forge:", "*.example:22", "!forge:22", "#forge:22", "|forge:22"} {
		_, err := app.TrustHost(context.Background(), scanner(key), app.TrustRequest{Address: address, Fingerprints: []string{ssh.FingerprintSHA256(key)}})
		expectRefusal(t, err, "trust")
	}
}

func TestRepinPreservesUnterminatedUnrelatedLine(t *testing.T) {
	t.Parallel()
	key := hostKey(t, 1)
	old := []byte(knownhosts.Line([]string{"other:22"}, key) + " exact comment")
	result, err := app.Repin(context.Background(), scanner(key), app.TrustRequest{Address: "forge:22", Fingerprints: []string{ssh.FingerprintSHA256(key)}}, old)
	if err != nil || !bytes.HasSuffix(result.Lines, old) {
		t.Fatal(string(result.Lines), err)
	}
}

func TestTrustConfirmationCannotAcceptAChangedKey(t *testing.T) {
	t.Parallel()
	first, changed := hostKey(t, 1), hostKey(t, 2)
	offered, err := app.TrustHost(context.Background(), scanner(first), app.TrustRequest{Address: "forge:22"})
	expectRefusal(t, err, "trust")
	result, err := app.TrustHost(context.Background(), scanner(changed), app.TrustRequest{Address: "forge:22", Fingerprints: offered.Offered})
	expectRefusal(t, err, "trust")
	if len(result.Lines) != 0 {
		t.Fatal("confirmation trusted a changed key")
	}
	confirmed, err := app.TrustHost(context.Background(), scanner(first), app.TrustRequest{Address: "forge:22", Fingerprints: offered.Offered})
	if err != nil || len(confirmed.Lines) == 0 {
		t.Fatal(confirmed, err)
	}
}
