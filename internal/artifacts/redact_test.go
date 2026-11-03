package artifacts

import (
	"errors"
	"testing"
)

func TestScanBytesForSecrets(t *testing.T) {
	for _, test := range []struct {
		name, body string
		patterns   []string
		found      bool
	}{
		{"secret", "prefix secret-value suffix", []string{"secret-value"}, true},
		{"clean", "clean", []string{"secret-value"}, false},
		{"empty patterns", "secret-value", nil, false},
		{"blank patterns", "secret-value", []string{"", ""}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := scanBytesForSecrets([]byte(test.body), test.patterns); errors.Is(err, ErrSecretDetected) != test.found {
				t.Fatalf("scan=%v", err)
			}
		})
	}
}

func TestTierGatedAllowsCIOnly(t *testing.T) {
	if !TierGated("ci") {
		t.Fatal("ci must collect artifacts")
	}
	for _, trigger := range []string{"release", "plan", "apply"} {
		if TierGated(trigger) {
			t.Fatalf("credentialed trigger %s collected artifacts", trigger)
		}
	}
}
