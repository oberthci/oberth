package setuptui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/oberthci/oberth/internal/installer"
)

// Plain mode asks the same questions: it lists the kubeconfig contexts and,
// when kind and docker exist, the kind choice (#011).
func TestPlainModeListsContextsAndKind(t *testing.T) {
	newTestHost(t)
	stubKindTools(t, true)
	answers := strings.Join([]string{
		"2",        // cluster: staging
		"", "", "", // namespaces
		"",     // network policy
		"",     // store
		"",     // anchoring
		"",     // tls names
		"",     // forge
		"acme", // organization
		"",     // upstream key
		"",     // identity
		"",     // uplink key
		"",     // client access
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := Run(context.Background(), Options{Plain: true, DryMode: true}, strings.NewReader(answers), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"Step 1 of 8 — Cluster", "1) default", "2) staging", "Create a local kind cluster",
		"Step 4 of 8 — Forge & upstream key", "--context='staging'", "--organization='acme'", "--client-access=both"} {
		if !strings.Contains(text, want) {
			t.Errorf("plain output missing %q:\n%s", want, text)
		}
	}
}

// Plain mode validates on the spot and asks again.
func TestPlainModeRejectsAndReasks(t *testing.T) {
	newTestHost(t)
	answers := strings.Join([]string{
		"9", "1", // an out-of-range choice, then a valid one
		"same", "same", "x", "", "", "", // duplicate namespaces, then defaults
		"", "", "", "", "",
		"acme/repo", "acme", // an invalid organization, then a valid one
		"", "", "", "",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := Run(context.Background(), Options{Plain: true, DryMode: true}, strings.NewReader(answers), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"ERROR: choose 1 to", "ERROR: the three namespaces must differ", "ERROR: an organization is one name"} {
		if !strings.Contains(text, want) {
			t.Errorf("plain output missing %q:\n%s", want, text)
		}
	}
}

// A plain install with a pending forge key exits partial with the exact
// commands, and the rerun offers Verify (#002, #003).
func TestPlainModeInstallAndVerify(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, keyPendingInstall)
	kubectl := stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})
	answers := strings.Repeat("\n", 9) + "acme\n" + strings.Repeat("\n", 4) + "yes\n"
	var out bytes.Buffer
	err := Run(context.Background(), Options{Plain: true}, strings.NewReader(answers), &out)
	if !errors.Is(err, installer.ErrOnboardingPartial) || !strings.Contains(out.String(), "oberth upstream add --yes --no-wait github") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}

	kubectl.acceptAdd = true
	out.Reset()
	if err := Run(context.Background(), Options{Plain: true}, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatalf("verify after registration must succeed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "✓ server Ready") {
		t.Fatalf("verify output:\n%s", out.String())
	}
}

func TestPlainModeStopsAtPromptWhenInterrupted(t *testing.T) {
	newTestHost(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{Plain: true}, pr, &out) }()
	select {
	case err := <-done:
		if !errors.Is(err, installer.ErrInterrupted) {
			t.Fatalf("interrupted plain setup must return ErrInterrupted, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plain setup kept waiting for input after ctrl+c")
	}
}

func TestReadLineStillDeliversLines(t *testing.T) {
	line, err := readLine(context.Background(), bufio.NewReader(strings.NewReader("hello\n")))
	if err != nil || line != "hello\n" {
		t.Fatalf("readLine = %q, %v", line, err)
	}
}

func TestRuntimeInterruptedClassifiesSignalsOnly(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{tea.ErrInterrupted, true},
		{fmt.Errorf("%w: %w", tea.ErrProgramKilled, context.Canceled), true},
		{fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrProgramPanic), false},
		{errors.New("render failed"), false},
	}
	for _, c := range cases {
		if got := runtimeInterrupted(c.err); got != c.want {
			t.Errorf("runtimeInterrupted(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestValidateStoreAddress(t *testing.T) {
	for addr, ok := range map[string]bool{
		"https://openbao.internal:8200": true,
		"http://openbao.internal:8200":  false,
		"HTTP://openbao.internal":       false,
		"openbao.internal:8200":         false,
		"":                              false,
		"https://":                      false,
	} {
		if err := validateStoreAddress(addr); (err == nil) != ok {
			t.Errorf("validateStoreAddress(%q) = %v", addr, err)
		}
	}
	if !errors.Is(validateStoreAddress("http://x"), errHTTPRejected) {
		t.Fatal("http:// must be rejected with the TLS invariant error")
	}
}

func TestCanonicalNetworkPolicy(t *testing.T) {
	for in, want := range map[string]string{"strict": "true", "auto": "auto", "off": "false", "true": "true"} {
		if got, ok := canonicalNetworkPolicy(in); !ok || got != want {
			t.Errorf("canonicalNetworkPolicy(%q) = %q", in, got)
		}
	}
	if _, ok := canonicalNetworkPolicy("lenient"); ok {
		t.Fatal("unknown policy accepted")
	}
}

func TestMaskerMaskAndWipe(t *testing.T) {
	m := newMasker()
	secret := []byte("tok-super-secret")
	m.register(secret)
	secret[0] = 'X'
	if masked := m.mask("a tok-super-secret b"); strings.Contains(masked, "tok-super-secret") {
		t.Fatalf("mask failed: %q", masked)
	}
	held := m.secrets[0]
	m.wipe()
	if !bytes.Equal(held, make([]byte, len(held))) {
		t.Fatal("wipe must zero the buffer")
	}
	m.register(nil)
	if len(m.secrets) != 0 {
		t.Fatal("empty secrets must not be registered")
	}
}

// A bare oberth_ line that bypassed the CredentialSink goes to the ceremony,
// never to the log.
func TestApplyWriterBackstop(t *testing.T) {
	ch := make(chan tea.Msg, 4)
	w := &applyWriter{ch: ch, masker: newMasker()}
	_, _ = w.Write([]byte("hello\n" + fakeBearer + "\npartial"))
	w.flush()
	close(ch)
	var got []tea.Msg
	for m := range ch {
		got = append(got, m)
	}
	if len(got) != 3 {
		t.Fatalf("messages: %#v", got)
	}
	if _, ok := got[1].(ceremonyTokenMsg); !ok {
		t.Fatalf("the token line must reach the ceremony: %#v", got[1])
	}
	if l, ok := got[2].(applyLogMsg); !ok || l.line != "partial" {
		t.Fatalf("an unterminated last line must be flushed: %#v", got[2])
	}
}

// The error persisted in setup.json is one masked, bounded line (review finding).
func TestPersistableErrorIsOneMaskedLine(t *testing.T) {
	m := newMasker()
	m.register([]byte("oberth_secret_token_value"))
	got := persistableError(m.mask("upstream add failed: oberth_secret_token_value\nfull log follows\nmore"))
	if strings.Contains(got, "oberth_secret_token_value") || strings.Contains(got, "\n") || !strings.HasPrefix(got, "upstream add failed") {
		t.Fatalf("persisted error = %q", got)
	}
	if long := persistableError(strings.Repeat("x", 1000)); len(long) > 310 {
		t.Fatalf("persisted error not bounded: %d", len(long))
	}
}
