package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallVerifyNamesSelectedProfileBeforeItsVerdict(t *testing.T) {
	for _, selection := range []string{"current", "profile", "context"} {
		t.Run(selection, func(t *testing.T) {
			a, profile := verificationProfile(t)
			env := a.env
			a.env = func(name string) string {
				if name == "OBERTH_PROFILE" {
					t.Error("installation verification read the ambient profile")
					return "another-profile"
				}
				return env(name)
			}
			profile.CAPin = "wrong"
			writeVerificationProfile(t, filepath.Dir(profile.CAFile), profile)
			var out, diagnostic bytes.Buffer
			a.out, a.diagnostic = &out, &diagnostic
			args := []string{"install", "--verify"}
			switch selection {
			case "current":
				if err := os.Symlink("verified", filepath.Join(a.base, "current")); err != nil {
					t.Fatal(err)
				}
			default:
				args = append(args, "--"+selection+"=verified")
			}
			if code := a.run(t.Context(), args); code != 1 || out.String() != "Verifying profile: verified\n" || !strings.Contains(diagnostic.String(), "Check the saved trust") {
				t.Fatal("verification hid its selected profile", code, out.String(), diagnostic.String())
			}
		})
	}
}

func TestInstallVerifyRefusesWithoutAProfileChoice(t *testing.T) {
	a, profile := verificationProfile(t)
	env := a.env
	a.env = func(name string) string {
		if name == "OBERTH_PROFILE" {
			t.Error("an ambient profile replaced an explicit installation choice")
			return "verified"
		}
		return env(name)
	}
	writeVerificationProfile(t, filepath.Dir(profile.CAFile), profile)
	if err := os.Mkdir(filepath.Join(a.base, "another"), 0700); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	a.out, a.diagnostic = &out, &diagnostic
	if code := a.run(t.Context(), []string{"install", "--verify"}); code != 1 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "Select an installation profile.") || !strings.Contains(diagnostic.String(), "--profile or --context") {
		t.Fatal("ambiguous verification tried an unnamed installation", code, out.String(), diagnostic.String())
	}
}
