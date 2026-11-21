package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func deliveredSigningKey(t *testing.T, env map[string]string) string {
	t.Helper()
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, workflow.GetTemplateByName("release-files-signed").Metadata.Annotations[argoworkflow.SecretPathsAnnotation])
	if err = os.MkdirAll(filepath.Dir(key), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	env["OBERTH_SECRETS_DIR"], env["COSIGN_KEY"] = dir, ""
	return key
}

func signingEnvironment(t *testing.T) map[string]string {
	t.Helper()
	env := releaseEnvironment(t)
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.Mkdir(dist, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"SHA256SUMS": "checksums"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureCommand(t, filepath.Join(dir, "signer"), `printf '%s\n' "$1" >> "$CALLS"
[ -z "${COSIGN_KEY:-}" ]
if [ "${COSIGN_PASSWORD+x}" != x ] || [ -n "${COSIGN_PASSWORD:-}" ]; then exit 1; fi
if IFS= read -r unexpected; then exit 1; fi
operation=$1
while [ $# -gt 0 ]; do case "$1" in --key) shift; key=$1;; --bundle) shift; bundle=$1;; --signing-config) shift; config=$1;; --trusted-root) shift; trusted=$1;; esac; shift; done
[ "$key" = "$EXPECTED_KEY" ] && [ -s "$key" ] || exit 1
case "$operation" in
public-key) [ -z "${KEY_READ_FAILURE:-}" ] || exit 1; printf '%s' "${SIGNING_PUBLIC_KEY:-pinned-public-key}";;
sign-blob)
python3 - "$config" "$trusted" <<'CONFIG'
import json, sys
value=json.load(open(sys.argv[1]))
assert value['mediaType']=='application/vnd.dev.sigstore.signingconfig.v0.2+json'
assert all(value.get(k)==[] for k in ('caUrls','oidcUrls','rekorTlogUrls','tsaUrls'))
assert json.load(open(sys.argv[2]))=={'mediaType':'application/vnd.dev.sigstore.trustedroot+json;version=0.1'}
CONFIG
[ -z "${SIGNING_FAILURE:-}" ] || exit 1
printf '%s' "${SIGNED_BUNDLE:-signed}" > "$bundle";;
*) exit 1;; esac
`)
	writeFixtureCommand(t, filepath.Join(dir, "curl"), `printf called >> "$CURL_CALLS"
exit 1
`)
	if err := os.MkdirAll(env["RELEASE_PRIVATE"], 0700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(dir, "signer"), filepath.Join(env["RELEASE_PRIVATE"], "cosign"))

	writeFixtureCommand(t, filepath.Join(dir, "sha256sum"), `read -r hash file
[ "$hash" = ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc ]
`)
	writeFixtureCommand(t, filepath.Join(dir, "tool"), `printf '%s\n' "$*" >> "$TOOL_CALLS"
operation=$1; shift
while [ $# -gt 0 ]; do case "$1" in --public-key-file) shift; public=$1;; --dir) shift; dir=$1;; esac; shift; done
case "$operation" in
key)
if [ "$(cat "$public")" != pinned-public-key ]; then
printf 'Signing key does not match the release pin.\nUse the private key for the committed release public key.\n' >&2; exit 1
fi
printf pinned-public-key;;
signature)
if [ -n "${VERIFY_FAILURE:-}" ] || { [ "$(cat "$dir/SHA256SUMS.sigstore.json")" != signed ]; }; then
printf '%s\n' 'Release signature does not verify.' "Fetch the owner's signed checksum manifest and matching signature." >&2; exit 1
fi;;
*) exit 1;; esac
`)

	env["PATH"], env["RELEASE_DIR"], env["RELEASE_TOOL"] = dir+string(os.PathListSeparator)+os.Getenv("PATH"), dist, filepath.Join(dir, "tool")
	env["SIGNER"], env["CALLS"], env["CURL_CALLS"] = filepath.Join(dir, "signer"), filepath.Join(dir, "calls"), filepath.Join(dir, "curl.calls")
	env["TOOL_CALLS"] = filepath.Join(dir, "tool.calls")
	env["EXPECTED_KEY"] = deliveredSigningKey(t, env)
	return env
}

func TestSigningReadsTheDeliveredKeyFile(t *testing.T) {
	env := signingEnvironment(t)
	env["COSIGN_PASSWORD"] = "ambient-password"
	if output, err := releaseScript(t, "sign", env); err != nil {
		t.Fatal(err, string(output))
	}
	body, err := os.ReadFile(filepath.Join(env["RELEASE_DIR"], "SHA256SUMS.sigstore.json"))
	if err != nil || string(body) != "signed" {
		t.Fatal("delivered key file was not used for signing", err)
	}
}

func TestSigningRefusesMissingDeliveredFileBeforeDownloads(t *testing.T) {
	for _, kind := range []string{"no-delivery", "directory", "file", "empty", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			env := signingEnvironment(t)
			invalidateSigningFile(t, env, kind)
			env["COSIGN_KEY"] = "fixture-environment-key"
			output, err := releaseScript(t, "sign", env)
			if err == nil || string(output) != "Signing key file is missing; provide the signing-step grant.\n" {
				t.Fatal("missing delivered file did not refuse", err, string(output))
			}
			if _, err := os.Stat(env["CURL_CALLS"]); !os.IsNotExist(err) {
				t.Fatal("download occurred without the key file", err)
			}
		})
	}
}

func invalidateSigningFile(t *testing.T, env map[string]string, kind string) {
	t.Helper()
	key := env["EXPECTED_KEY"]
	switch kind {
	case "no-delivery":
		env["OBERTH_SECRETS_DIR"] = ""
	case "file":
		env["OBERTH_SECRETS_DIR"] = t.TempDir()
	case "directory":
		if err := os.Remove(key); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(key, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(key, "child"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	case "empty":
		if err := os.WriteFile(key, nil, 0600); err != nil {
			t.Fatal(err)
		}
	case "unreadable":
		if os.Geteuid() == 0 {
			t.Skip("root can read files without read permissions")
		}
		if err := os.Chmod(key, 0000); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSigningRefusesBundleFromUnrelatedKey(t *testing.T) {
	env := signingEnvironment(t)
	env["SIGNED_BUNDLE"] = "unrelated-signature"
	output, err := releaseScript(t, "sign", env)
	want := releaseverify.Signature.Error() + "\n" + releaseverify.Signature.Remedy() + "\n"
	if err == nil || string(output) != want {
		t.Fatal("unrelated signature was accepted", err, string(output))
	}
}

func TestSigningRefusesWrongKeyBeforeSigning(t *testing.T) {
	env := signingEnvironment(t)
	env["SIGNING_PUBLIC_KEY"] = "another-public-key"
	output, err := releaseScript(t, "sign", env)
	want := signingKeyMismatch.Error() + "\n" + signingKeyMismatch.Remedy() + "\n"
	if err == nil || string(output) != want {
		t.Fatal("unrelated key was accepted", err, string(output))
	}
	calls, err := os.ReadFile(env["CALLS"])
	if err != nil || string(calls) != "public-key\n" {
		t.Fatal("mismatched key reached signing", err)
	}
	curl, err := os.ReadFile(env["CURL_CALLS"])
	if !os.IsNotExist(err) || len(curl) != 0 {
		t.Fatal("mismatched key made a network request", err)
	}
}

func TestSigningToolFailureHasCompleteRemedy(t *testing.T) {
	for _, failure := range []string{"checksum", "output"} {
		t.Run(failure, func(t *testing.T) {
			env := signingEnvironment(t)
			env["SIGNING_FAILURE"] = failure
			output, err := releaseScript(t, "sign", env)
			if err == nil || string(output) != "Signing failed; check the checksum file and release directory.\n" {
				t.Fatal("signing failure lacks whole guidance", err, string(output))
			}
		})
	}
}

func TestFreshVerificationFailureHasCompleteRemedy(t *testing.T) {
	env := signingEnvironment(t)
	env["VERIFY_FAILURE"] = "signature"
	output, err := releaseScript(t, "sign", env)
	want := releaseverify.Signature.Error() + "\n" + releaseverify.Signature.Remedy() + "\n"
	if err == nil || string(output) != want {
		t.Fatal("fresh signature bypassed the product check", err, string(output))
	}
	calls, err := os.ReadFile(env["CALLS"])
	if err != nil || string(calls) != "public-key\nsign-blob\n" {
		t.Fatal("signer was used for verification", err)
	}
}

func TestSigningUnreadableKeyRefusesBeforeSigning(t *testing.T) {
	env := signingEnvironment(t)
	env["KEY_READ_FAILURE"] = "yes"
	output, err := releaseScript(t, "sign", env)
	if err == nil || string(output) != "Signing key cannot be read; use a key with an empty password.\n" {
		t.Fatal("private key read failure lacks whole recovery", err, string(output))
	}
	calls, err := os.ReadFile(env["CURL_CALLS"])
	if !os.IsNotExist(err) || len(calls) != 0 {
		t.Fatal("unreadable key made a network request", err)
	}
}

func TestSigningKeepsThePinCheckWithoutSavingItsPublicOutput(t *testing.T) {
	env := signingEnvironment(t)
	if output, err := releaseScript(t, "sign", env); err != nil {
		t.Fatal(err, string(output))
	}
	if _, err := os.Stat(filepath.Join(env["RELEASE_PRIVATE"], "release.pub")); !os.IsNotExist(err) {
		t.Fatal("signing left an unused public-key copy", err)
	}
	calls, err := os.ReadFile(env["TOOL_CALLS"])
	if err != nil || !strings.Contains(string(calls), "key --public-key-file ") {
		t.Fatal("signing skipped the compiled-pin comparison", err)
	}
}
