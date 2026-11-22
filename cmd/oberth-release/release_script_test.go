package main

import (
	"context"
	"github.com/oberthci/oberth/internal/releaseverify"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func releaseScript(t *testing.T, operation string, values map[string]string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "../../hack/release.sh", operation)
	command.Env = scriptEnv()
	for key, value := range values {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}
func releaseEnvironment(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(filepath.Join(secrets, "oberth/data/release"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "oberth/data/release/github-token"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"VERSION": "v1.2.3", "RELEASE_ORIGIN": releaseverify.Origin, "OBERTH_SECRETS_DIR": secrets, "RELEASE_PRIVATE": filepath.Join(dir, "private"), "RELEASE_SHARED": filepath.Join(dir, "shared"), "RELEASE_VERIFY_DIR": filepath.Join(dir, "verify"), "RELEASE_DIR": filepath.Join(dir, "dist"), "RELEASE_TOOL": filepath.Join(dir, "oberth-release")}
}
func TestReleaseScriptRefusals(t *testing.T) {
	for _, tc := range []struct{ name, key, value, reason string }{
		{"missing token", "OBERTH_SECRETS_DIR", "", "token is missing"},
		{"bad tag", "VERSION", "v1.2.3; command", "tag is invalid"},
		{"non-HTTPS origin", "RELEASE_ORIGIN", "http://release.example.test", "origin is not HTTPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := releaseEnvironment(t)
			env[tc.key] = tc.value
			output, err := releaseScript(t, "publish", env)
			if err == nil || strings.Count(string(output), "\n") != 1 || len(strings.TrimSpace(string(output))) > 80 || !strings.Contains(string(output), tc.reason) {
				t.Fatalf("refusal: %v %s", err, output)
			}
		})
	}
}
func TestReleaseRequiresTestedDefaultHistory(t *testing.T) {
	command := exec.Command("git", "rev-parse", "HEAD")
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		allowed bool
	}{{"valid", true}, {"ancestor", true}, {"missing", false}, {"default", true}, {"tested", false}, {"status", false}, {"tag", false}} {
		which := tc.name
		t.Run(which, func(t *testing.T) {
			env := releaseEnvironment(t)
			directory := t.TempDir()
			git := filepath.Join(directory, "git")
			if err := os.WriteFile(git, []byte("#!/bin/sh\ncase \"$*\" in *HEAD*) printf '%s\\n' \"$HEAD_COMMIT\";; *) printf '%s\\n' \"$TAG_COMMIT\";; esac\n"), 0700); err != nil {
				t.Fatal(err)
			}
			env["PATH"] = directory + string(os.PathListSeparator) + os.Getenv("PATH")
			env["HEAD_COMMIT"], env["TAG_COMMIT"] = strings.TrimSpace(string(head)), strings.TrimSpace(string(head))
			env["RELEASE_DEFAULT_COMMIT"], env["RELEASE_TESTED_COMMIT"], env["RELEASE_BUILD_STATUS"] = strings.TrimSpace(string(head)), strings.TrimSpace(string(head)), "passed"
			switch which {
			case "missing":
				env["RELEASE_TESTED_COMMIT"] = ""
			case "ancestor":
				env["RELEASE_DEFAULT_COMMIT"] = strings.Repeat("b", 40)
			case "default":
				env["RELEASE_DEFAULT_COMMIT"] = ""
			case "tested":
				env["RELEASE_TESTED_COMMIT"] = strings.Repeat("b", 40)
			case "status":
				env["RELEASE_BUILD_STATUS"] = "running"
			case "tag":
				env["TAG_COMMIT"] = strings.Repeat("b", 40)
			}
			output, err := releaseScript(t, "check", env)
			if (err == nil) != tc.allowed {
				t.Fatalf("eligibility: %v %s", err, output)
			}
		})
	}
}

// The publish and finalize steps hand the granted GitHub token file to the
// tool by path; the script reads no token itself.
func TestReleaseScriptPassesTheGrantedGitHubToken(t *testing.T) {
	for _, operation := range []string{"publish", "finalize"} {
		env := releaseEnvironment(t)
		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		writeFixtureCommand(t, filepath.Join(dir, "tool"), `printf '%s\n' "$*" > "$TOOL_CALLS"
`)
		env["RELEASE_TOOL"], env["TOOL_CALLS"] = filepath.Join(dir, "tool"), calls
		output, err := releaseScript(t, operation, env)
		if err != nil {
			t.Fatal(operation, err, string(output))
		}
		body, err := os.ReadFile(calls)
		want := operation + " --dir " + env["RELEASE_DIR"] + " --version v1.2.3 --origin " + releaseverify.Origin + " --token-file " + env["OBERTH_SECRETS_DIR"] + "/oberth/data/release/github-token --receipt " + env["RELEASE_SHARED"] + "/verified.json\n"
		if err != nil || string(body) != want {
			t.Fatal("tool did not receive the granted token path", operation, string(body), err)
		}
	}
}

func TestReleaseRefusalCatalogue(t *testing.T) {
	body, err := os.ReadFile("../../hack/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		_, reason, ok := strings.Cut(line, "fail '")
		if !ok {
			continue
		}
		for i, message := range regexp.MustCompile("'([^']*)'").FindAllStringSubmatch("'"+reason, -1) {
			limit := 80
			if i > 0 {
				limit = 120
			}
			if len(message[1]) > limit || strings.Contains(message[1], "...") {
				t.Fatal("bad refusal or remedy", message[1])
			}
		}
	}
}

func TestSigningIsOfflineWithoutAReleaseOrigin(t *testing.T) {
	env := signingEnvironment(t)
	env["RELEASE_ORIGIN"] = ""
	output, err := releaseScript(t, "sign", env)
	if err != nil {
		t.Fatal("offline signing failed", err, string(output))
	}
	if _, err := os.Stat(env["CURL_CALLS"]); !os.IsNotExist(err) {
		t.Fatal("signing attempted network access", err)
	}
	body, err := os.ReadFile(filepath.Join(env["RELEASE_DIR"], "SHA256SUMS.sigstore.json"))
	if err != nil || string(body) != "signed" {
		t.Fatal("offline signature missing", err)
	}
	calls, err := os.ReadFile(env["TOOL_CALLS"])
	if err != nil || !strings.Contains(string(calls), "signature --dir "+env["RELEASE_DIR"]) {
		t.Fatal("offline signature was not checked with the pinned key", err)
	}
}

func TestSigningRequiresThePreparedExecutable(t *testing.T) {
	env := signingEnvironment(t)
	if err := os.Remove(filepath.Join(env["RELEASE_PRIVATE"], "cosign")); err != nil {
		t.Fatal(err)
	}
	output, err := releaseScript(t, "sign", env)
	if err == nil || string(output) != "Signing tool is missing.\nPlace the pinned executable at RELEASE_PRIVATE/cosign before signing offline.\n" {
		t.Fatal("missing offline tool lacks preparation remedy", err, string(output))
	}
	if _, err := os.Stat(env["CURL_CALLS"]); !os.IsNotExist(err) {
		t.Fatal("missing tool triggered a network request", err)
	}
}
func writeFixtureCommand(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseScriptRejectsDifferentPublicOriginBeforeEffects(t *testing.T) {
	env := releaseEnvironment(t)
	env["RELEASE_ORIGIN"] = "https://other.oberth.ci/oberth"
	bin := t.TempDir()
	marker := filepath.Join(bin, "contacted")
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nprintf contacted > \"$CONTACT_MARKER\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env["PATH"], env["CONTACT_MARKER"] = bin+string(os.PathListSeparator)+os.Getenv("PATH"), marker
	output, err := releaseScript(t, "publish", env)
	if err == nil || !strings.Contains(string(output), "Release origin differs") {
		t.Fatal("alternate publication address was not refused", err, string(output))
	}
	for _, path := range []string{marker, env["RELEASE_PRIVATE"]} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("origin refusal happened after side effects", path, err)
		}
	}
}
