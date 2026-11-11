package releaseverify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Only HTTP transport is replaced. The script runs real OpenSSL and checksum
// tools on a minimal PATH without Python, jq, cosign or a preinstalled Oberth.
func TestPluginFetchVerifiesBeforeInstalling(t *testing.T) {
	for _, scenario := range []string{"valid", "latest", "wget-shasum", "binary", "checksums", "signature", "wrong-key", "bad-version", "wrong-release", "duplicate", "path"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "wget-shasum" {
				if _, err := exec.LookPath("shasum"); err != nil {
					t.Skip("shasum alternative requires the host shasum utility; sha256sum is covered separately")
				}
			}
			root := t.TempDir()
			write := func(name string, body []byte, mode os.FileMode) {
				t.Helper()
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, mode); err != nil {
					t.Fatal(err)
				}
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			write("plugin/pins/release.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0600)
			script, err := os.ReadFile("../../claude-plugin/scripts/fetch-oberth")
			if err != nil {
				t.Fatal(err)
			}
			write("plugin/scripts/fetch-oberth", script, 0700)
			tools := []string{"openssl", "awk", "mktemp", "uname", "dirname", "mkdir", "rm", "wc", "cat", "chmod", "mv", "ln"}
			if scenario == "wget-shasum" {
				tools = append(tools, "shasum")
			} else {
				tools = append(tools, "sha256sum")
			}
			for _, tool := range tools {
				path, e := exec.LookPath(tool)
				if e != nil {
					t.Fatalf("test prerequisite %s: %v", tool, e)
				}
				if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(root, "bin", tool)); err != nil {
					t.Fatal(err)
				}
			}
			transport := `#!/bin/sh
set -eu
out= url=
while [ "$#" -gt 0 ]; do
 case "$1" in
 --output|-O) out=$2; shift 2;;
 --proto|--proto-redir|--connect-timeout|--max-time|--max-filesize) shift 2;;
 --*) shift;;
 *) url=$1; shift;;
 esac
done
case "$url" in https://releases.oberth.ci/oberth/*) ;; *) exit 1;; esac
cat "$FIXTURES/${url##*/}" >"$out"
`
			downloader := "curl"
			if scenario == "wget-shasum" {
				downloader = "wget"
			}
			write("bin/"+downloader, []byte(transport), 0700)
			var checks strings.Builder
			for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "oberth-1.2.3.tgz"} {
				body := []byte("fixture artifact never executed\n")
				write("fixtures/"+name, body, 0600)
				fmt.Fprintf(&checks, "%x  %s\n", sha256.Sum256(body), name)
			}
			manifest := checks.String()
			if scenario == "duplicate" {
				manifest = strings.Replace(manifest, "oberth-linux-arm64", "oberth-linux-amd64", 1)
			}
			if scenario == "path" {
				manifest = strings.Replace(manifest, "oberth-linux-amd64", "../oberth-linux-amd64", 1)
			}
			payload := []byte(manifest)
			digest := sha256.Sum256(payload)
			signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			write("fixtures/SHA256SUMS", payload, 0600)
			write("fixtures/SHA256SUMS.sig", signature, 0600)
			write("fixtures/VERSION", []byte("v1.2.3\n"), 0600)
			switch scenario {
			case "binary":
				for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64"} {
					write("fixtures/"+name, []byte("tampered"), 0600)
				}
			case "checksums":
				write("fixtures/SHA256SUMS", append(payload, []byte("tampered")...), 0600)
			case "signature":
				write("fixtures/SHA256SUMS.sig", []byte("tampered"), 0600)
			case "wrong-key":
				other, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if e != nil {
					t.Fatal(e)
				}
				der, e := x509.MarshalPKIXPublicKey(&other.PublicKey)
				if e != nil {
					t.Fatal(e)
				}
				write("plugin/pins/release.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600)
			}
			args := []string{"v1.2.3"}
			if scenario == "latest" {
				args = nil
			}
			if scenario == "bad-version" {
				args = []string{"v1.2.3;touch injected"}
			}
			if scenario == "wrong-release" {
				args = []string{"v1.2.4"}
			}
			cmd := exec.Command(filepath.Join(root, "plugin/scripts/fetch-oberth"), args...)
			cmd.Env = []string{"PATH=" + filepath.Join(root, "bin"), "HOME=" + filepath.Join(root, "home"), "FIXTURES=" + filepath.Join(root, "fixtures")}
			out, err := cmd.CombinedOutput()
			success := scenario == "valid" || scenario == "latest" || scenario == "wget-shasum"
			if (err == nil) != success {
				t.Fatalf("result err=%v: %s", err, out)
			}
			target := filepath.Join(root, "home/.local/bin/oberth")
			if success {
				dest, e := os.Readlink(target)
				if e != nil || dest != "oberth-v1.2.3" {
					t.Fatalf("incorrect installation: %s %v", dest, e)
				}
			} else if _, e := os.Lstat(target); !os.IsNotExist(e) {
				t.Fatal("failed verification installed a binary")
			}
		})
	}
}

// Run the pipeline's exact extraction helper with real ECDSA/OpenSSL, including
// mismatched bundle/signature and changed-payload refusals.
func TestReleaseDetachedSignatureMatchesBundle(t *testing.T) {
	root := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("signed checksums fixture\n")
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("release.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))
	write("SHA256SUMS", payload)
	bundle, err := json.Marshal(map[string]any{"messageSignature": map[string]string{"signature": base64.StdEncoding.EncodeToString(signature)}})
	if err != nil {
		t.Fatal(err)
	}
	write("SHA256SUMS.sigstore.json", bundle)
	source, err := os.ReadFile("../../.oberth/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "export_checksum_signature() {")
	end := strings.Index(string(source), "\nsign_binaries() {")
	if start < 0 || end < start {
		t.Fatal("missing extraction helper")
	}
	write("extract.sh", []byte("set -eu\nfail() { exit 1; }\nrelease_public_key=$1/release.pub\n"+string(source[start:end])+"\nexport_checksum_signature \"$1\" \"$2\"\n"))
	run := func(mode string) error {
		cmd := exec.Command("/bin/sh", filepath.Join(root, "extract.sh"), root, mode)
		return cmd.Run()
	}
	if err := run("write"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "SHA256SUMS.sig"))
	if err != nil || !bytes.Equal(actual, signature) {
		t.Fatal("extraction did not preserve bundle signature")
	}
	if err := run("check"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	write("SHA256SUMS.sig", []byte("different signature"))
	if run("check") == nil {
		t.Fatal("mismatched signature accepted")
	}
	write("SHA256SUMS.sig", signature)
	write("SHA256SUMS", []byte("tampered"))
	if run("check") == nil {
		t.Fatal("changed payload accepted")
	}
}

func TestPluginInstallRequiresRecoveryFile(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"bin/helm", "bin/kubectl", ".local/bin/oberth"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'installer-called\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs("../../claude-plugin/scripts/install")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run"}, {"run", "--secrets-file", ""}, {"run", "--secrets-file="}, {"run", "--secrets-file"}, {"run", "--secrets-file", "--yes"}, {"run", "--secrets-file", "recovery.txt"}} {
		cmd := exec.Command(script, args...)
		cmd.Env = []string{"HOME=" + root, "PATH=" + filepath.Join(root, "bin")}
		out, err := cmd.CombinedOutput()
		want := args[len(args)-1] == "recovery.txt"
		if (err == nil) != want || strings.Contains(string(out), "installer-called") != want {
			t.Fatalf("wrapper accepted invalid recovery path: %v", args)
		}
	}
}
