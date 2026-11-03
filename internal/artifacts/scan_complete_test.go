package artifacts

import (
	"errors"
	"strings"
	"testing"
)

func TestExtractScansSecretsAfter64MiB(t *testing.T) {
	store, root := newStore(t)
	secret := "secret-beyond-the-old-prefix-limit"
	body := strings.Repeat("clean padding\n", (64<<20)/14+1) + secret
	_, err := store.Extract("large", archiveOf(t, member{name: "report.txt", body: body}), 128<<20, []string{secret})
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("late secret was not refused: %v", err)
	}
	assertNothingWritten(t, root)
}

func TestExtractAcceptsCleanLongLine(t *testing.T) {
	store, _ := newStore(t)
	body := strings.Repeat("x", 2<<20)
	if _, err := store.Extract("long", archiveOf(t, member{name: "report.json", body: body}), 4<<20, DefaultScanPatterns); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadAll("long", "report.json")
	if err != nil || string(got) != body {
		t.Fatalf("stored body changed: %v", err)
	}
}

func TestExtractFindsMultilineSecret(t *testing.T) {
	store, root := newStore(t)
	secret := "first line\nsecond line"
	_, err := store.Extract("multiline", archiveOf(t, member{name: "report", body: "before " + secret + " after"}), 1<<20, []string{secret})
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("multiline secret not refused: %v", err)
	}
	assertNothingWritten(t, root)
}
