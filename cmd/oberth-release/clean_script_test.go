package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func signedDownloads(t *testing.T) (map[string][]byte, []byte) {
	t.Helper()
	return signedDownloadFiles(t, nil)
}

func signedDownloadFiles(t *testing.T, overrides map[string][]byte) (map[string][]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	names := publishedNames()
	slices.Sort(names)
	var sums strings.Builder
	for _, name := range names {
		files[name] = []byte("fixture " + name)
		if name == "release.json" {
			files[name] = []byte(fmt.Sprintf(`{"version":"v1.2.3","commit":"%s","images":{"oberth":"sha256:%s","oberth-source-plugin":"sha256:%s"},"base":"sha256:%s"}`, strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)))
		}
		if body, ok := overrides[name]; ok {
			files[name] = body
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	digest := sha256.Sum256(files["SHA256SUMS"])
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	files["SHA256SUMS.sigstore.json"], err = json.Marshal(map[string]any{"messageSignature": map[string]any{"signature": signature}})
	if err != nil {
		t.Fatal(err)
	}
	return files, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
func cleanScriptEnvironment(t *testing.T, files map[string][]byte, public []byte) map[string]string {
	t.Helper()
	env := releaseEnvironment(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, path := range []string{bin, filepath.Join(dir, "local"), filepath.Join(dir, "verify")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, "local", name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureCommand(t, filepath.Join(bin, "curl"), `exec python3 - "$@" <<'PY_DOWNLOAD'
import os, sys
origin, asset = os.environ["PUBLIC_ORIGIN"], "https://release-assets.githubusercontent.com/"
def address(value):
    if value.startswith(origin + "/"):
        return os.environ["DOWNLOAD_ORIGIN"] + value[len(origin):]
    if value.startswith(asset):
        return os.environ["DOWNLOAD_ORIGIN"] + "/asset/" + value[len(asset):]
    return value
arguments = [address(value) for value in sys.argv[1:]]
os.execv(os.environ["CURL_REAL"], [os.environ["CURL_REAL"], *arguments])
PY_DOWNLOAD
`)
	tool := filepath.Join(bin, "tool")
	writeFixtureCommand(t, tool, `case "$1" in
signature) printf '%s\n' "$*" >> "$SIGNATURE_CALLS"; exec "$VERIFIER" "$@";;
verify)
shift
while [ $# -gt 0 ]; do case "$1" in --dir) shift; dir=$1;; --receipt) shift; receipt=$1;; esac; shift; done
printf '%s\n' "$dir" > "$TOOL_DIRECTORY"
(cd "$dir"; "$SHA_REAL" -c SHA256SUMS >/dev/null)
printf verified > "$receipt";; *) exit 1;; esac
`)
	env["PATH"], env["RELEASE_DIR"], env["RELEASE_SHARED"], env["RELEASE_VERIFY_DIR"], env["RELEASE_TOOL"] = bin+":"+os.Getenv("PATH"), filepath.Join(dir, "local"), dir, filepath.Join(dir, "verify"), tool
	env["VERIFIER"] = compiledReleaseTool(t, public)
	env["SIGNATURE_CALLS"], env["TOOL_DIRECTORY"] = filepath.Join(dir, "signature.calls"), filepath.Join(dir, "tool.directory")
	for key, name := range map[string]string{"CURL_REAL": "curl", "SHA_REAL": "sha256sum"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		env[key] = path
	}
	return env
}
func TestCleanVerificationUsesAuthenticatedDownloads(t *testing.T) {
	const asset = "https://release-assets.githubusercontent.com/"
	for _, kind := range []string{"good", "signature", "payload", "asset host", "asset host payload", "other host"} {
		t.Run(kind, func(t *testing.T) {
			files, public := signedDownloads(t)
			env := cleanScriptEnvironment(t, files, public)
			switch kind {
			case "signature":
				files["SHA256SUMS.sigstore.json"] = []byte(`{"messageSignature":{"signature":"YmFk"}}`)
			case "payload", "asset host payload":
				files["oberth-linux-amd64"] = []byte("tampered download")
			}
			redirect := map[string]string{"asset host": asset, "asset host payload": asset, "other host": "https://example.test/"}[kind]
			configureDownloadOrigin(t, files, env, redirect)
			output, err := releaseScript(t, "verify", env)
			if (err == nil) != (kind == "good" || kind == "asset host") {
				t.Fatal("clean verification result", kind, err, string(output))
			}
			if kind == "other host" && !strings.Contains(string(output), "Target: https://example.test/SHA256SUMS") {
				t.Fatal("refused redirect hides its target", string(output))
			}
			if err == nil {
				assertCleanDirectory(t, env)
			} else if _, err := os.Stat(filepath.Join(env["RELEASE_SHARED"], "verified.json")); !os.IsNotExist(err) {
				t.Fatal("failed download created a receipt", err)
			}
		})
	}
}

// configureDownloadOrigin serves the files below the version path or, with a
// redirect prefix, answers there with a redirect to prefix+name as GitHub does.
func configureDownloadOrigin(t *testing.T, files map[string][]byte, env map[string]string, redirect string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, versioned := strings.CutPrefix(r.URL.Path, "/v1.2.3/")
		if redirect != "" && versioned {
			http.Redirect(w, r, redirect+name, http.StatusFound)
			return
		}
		if redirect != "" {
			name = strings.TrimPrefix(r.URL.Path, "/asset/")
		}
		body, ok := files[name]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(env["RELEASE_SHARED"], "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	env["PUBLIC_ORIGIN"], env["DOWNLOAD_ORIGIN"], env["CURL_CA_BUNDLE"] = env["RELEASE_ORIGIN"], server.URL, ca
	env["NO_PROXY"], env["no_proxy"] = "127.0.0.1,localhost", "127.0.0.1,localhost"
}
func assertCleanDirectory(t *testing.T, env map[string]string) {
	t.Helper()
	body, err := os.ReadFile(env["TOOL_DIRECTORY"])
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(body))
	if !strings.HasPrefix(dir, env["RELEASE_VERIFY_DIR"]+"/") {
		t.Fatal("verified local build instead of downloads", dir)
	}
	calls, err := os.ReadFile(env["SIGNATURE_CALLS"])
	if err != nil {
		t.Fatal("signature check omitted", err)
	}
	if !strings.Contains(string(calls), "signature --dir") || !strings.Contains(string(calls), dir) || strings.Contains(string(calls), "insecure-ignore") {
		t.Fatal("wrong signature verification arguments", string(calls))
	}
}
