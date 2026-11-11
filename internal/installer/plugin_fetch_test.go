package installer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the production shell with real OpenSSL and hashing and a PATH
// containing only the stated prerequisites. Only the release transport and
// platform detection are substituted; the test key lives in a private copy.
func TestPluginFetchVerifiedRelease(t *testing.T) {
	requireOpenSSL(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	for _, shell := range trustShells() {
		for _, variant := range []string{"latest", "requested", "wget", "wget-shasum", "linux-arm64", "darwin-amd64", "darwin-arm64", "binary", "checksums", "signature", "missing-signature", "wrong-key", "key-pin", "duplicate", "path", "wrong-chart", "missing-binary", "bad-version", "download-failure"} {
			t.Run(shell+"/"+variant, func(t *testing.T) {
				if variant == "wget-shasum" {
					if _, err := exec.LookPath("shasum"); err != nil {
						t.Skip("optional shasum path is tested on macOS; this host provides sha256sum")
					}
				}
				root := t.TempDir()
				write := func(path string, body []byte, mode os.FileMode) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, body, mode); err != nil {
						t.Fatal(err)
					}
				}
				script, err := os.ReadFile(pluginPath(t, "scripts", "fetch-oberth"))
				if err != nil {
					t.Fatal(err)
				}
				script = []byte(strings.ReplaceAll(string(script), "036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31", fmt.Sprintf("%x", sha256.Sum256(public))))
				scriptPath := filepath.Join(root, "plugin", "scripts", "fetch-oberth")
				write(scriptPath, script, 0700)
				keyPath := filepath.Join(root, "plugin", "pins", "release-cosign.pub")
				write(keyPath, public, 0600)
				tools := filepath.Join(root, "tools")
				commands := []string{"openssl", "awk", "grep", "mktemp", "dirname", "cat", "wc", "rm", "rmdir", "mkdir", "cp", "chmod", "ln", "mv"}
				downloader, hasher := "curl", "sha256sum"
				if strings.HasPrefix(variant, "wget") {
					downloader = "wget"
				}
				if variant == "wget-shasum" {
					hasher = "shasum"
				}
				commands = append(commands, hasher)
				for _, name := range commands {
					path, err := exec.LookPath(name)
					if err != nil {
						t.Fatalf("test prerequisite %s is missing", name)
					}
					if err := os.MkdirAll(tools, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
						t.Fatal(err)
					}
				}
				platform, arch := "Linux", "x86_64"
				if strings.HasPrefix(variant, "darwin") {
					platform = "Darwin"
				}
				if strings.HasSuffix(variant, "arm64") {
					arch = "arm64"
				}
				write(filepath.Join(tools, "uname"), []byte(fmt.Sprintf("#!/bin/sh\ncase $1 in -s) echo %s;; -m) echo %s;; *) exit 1;; esac\n", platform, arch)), 0700)
				transport := `#!/bin/sh
set -eu
out= url=
while [ "$#" -gt 0 ]; do
 case "$1" in
 --output|-O) out=$2; shift 2;;
 --proto|--proto-redir|--tlsv1.2|--connect-timeout|--max-time|--max-filesize) if [ "$1" = --tlsv1.2 ]; then shift; else shift 2; fi;;
 https://releases.oberth.ci/oberth/*) url=$1; shift;;
 *) shift;;
 esac
done
[ -n "$url" ] && [ -n "$out" ]
[ ! -f "$FIXTURE_RELEASE/fail" ]
case "$url" in */latest/VERSION) name=VERSION;; */v1.2.3/*) name=${url##*/};; *) exit 1;; esac
cp "$FIXTURE_RELEASE/$name" "$out"
`
				write(filepath.Join(tools, downloader), []byte(transport), 0700)
				release := filepath.Join(root, "release")
				binary := []byte("verified binary bytes; never executed during fetch\n")
				var sums strings.Builder
				for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "oberth-1.2.3.tgz"} {
					body := append([]byte(name+"\n"), binary...)
					write(filepath.Join(release, name), body, 0600)
					fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(body), name)
				}
				osName, archName := strings.ToLower(platform), "amd64"
				if arch == "arm64" {
					archName = arch
				}
				artifact := "oberth-" + osName + "-" + archName
				checksumBody := sums.String()
				switch variant {
				case "duplicate":
					checksumBody += strings.Split(checksumBody, "\n")[0] + "\n"
				case "path":
					checksumBody = strings.Replace(checksumBody, artifact, "../"+artifact, 1)
				case "wrong-chart":
					checksumBody = strings.ReplaceAll(checksumBody, "oberth-1.2.3.tgz", "oberth-9.9.9.tgz")
				case "missing-binary":
					checksumBody = strings.Replace(checksumBody, artifact, "oberth-other", 1)
				}
				write(filepath.Join(release, "SHA256SUMS"), []byte(checksumBody), 0600)
				digest := sha256.Sum256([]byte(checksumBody))
				signer := key
				if variant == "wrong-key" {
					signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
				}
				signature, err := ecdsa.SignASN1(rand.Reader, signer, digest[:])
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(release, "SHA256SUMS.sig"), signature, 0600)
				write(filepath.Join(release, "VERSION"), []byte("v1.2.3\n"), 0600)
				switch variant {
				case "binary":
					write(filepath.Join(release, artifact), []byte("tampered"), 0600)
				case "checksums":
					write(filepath.Join(release, "SHA256SUMS"), []byte("tampered"), 0600)
				case "signature":
					write(filepath.Join(release, "SHA256SUMS.sig"), []byte("tampered"), 0600)
				case "missing-signature":
					if err := os.Remove(filepath.Join(release, "SHA256SUMS.sig")); err != nil {
						t.Fatal(err)
					}
				case "key-pin":
					write(keyPath, append(public, '\n'), 0600)
				case "bad-version":
					write(filepath.Join(release, "VERSION"), []byte("v1.2.3/../../escape\n"), 0600)
				case "download-failure":
					write(filepath.Join(release, "fail"), nil, 0600)
				}
				home := filepath.Join(root, "home")
				bin := filepath.Join(home, ".local", "bin")
				write(filepath.Join(bin, "oberth-v0.0.1"), []byte("previous verified install"), 0700)
				if err := os.Symlink("oberth-v0.0.1", filepath.Join(bin, "oberth")); err != nil {
					t.Fatal(err)
				}
				args := []string{scriptPath}
				if variant == "requested" {
					args = append(args, "v1.2.3")
				}
				cmd := exec.CommandContext(t.Context(), shell, args...)
				cmd.Env = []string{"PATH=" + tools, "HOME=" + home, "TMPDIR=" + root, "FIXTURE_RELEASE=" + release}
				output, err := cmd.CombinedOutput()
				success := variant == "latest" || variant == "requested" || strings.HasPrefix(variant, "wget") || strings.HasPrefix(variant, "linux-") || strings.HasPrefix(variant, "darwin-")
				if success {
					if err != nil {
						t.Fatalf("fetch: %v\n%s", err, output)
					}
					got := mustRead(t, filepath.Join(bin, "oberth-v1.2.3"))
					if string(got) != string(mustRead(t, filepath.Join(release, artifact))) {
						t.Fatal("installed bytes differ")
					}
					info, err := os.Stat(filepath.Join(bin, "oberth-v1.2.3"))
					if err != nil || info.Mode().Perm() != 0755 {
						t.Fatalf("binary permissions: %v %v", info, err)
					}
					target, err := os.Readlink(filepath.Join(bin, "oberth"))
					if err != nil || target != "oberth-v1.2.3" {
						t.Fatalf("symlink: %q %v", target, err)
					}
				} else {
					if err == nil || !strings.Contains(string(output), "fetch-oberth:") {
						t.Fatalf("unsafe release accepted: %v %s", err, output)
					}
					target, err := os.Readlink(filepath.Join(bin, "oberth"))
					if err != nil || target != "oberth-v0.0.1" {
						t.Fatalf("existing installation changed: %q %v", target, err)
					}
					if _, err := os.Stat(filepath.Join(bin, "oberth-v1.2.3")); !os.IsNotExist(err) {
						t.Fatal("refusal installed a binary")
					}
				}
			})
		}
	}
}

func TestPluginCommandsOnlyAllowOwnScripts(t *testing.T) {
	for skill, script := range map[string]string{"install": "fetch-oberth", "connect": "trust", "unseal": "unseal"} {
		body := string(mustRead(t, pluginPath(t, "skills", skill, "SKILL.md")))
		want := `allowed-tools: Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/` + script + `" *)`
		if skill == "install" {
			want += `, Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" *)`
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "allowed-tools:") && line != want {
				t.Fatalf("%s grants an unexpected tool: %s", skill, line)
			}
		}
		if !strings.Contains(body, want) {
			t.Fatalf("%s lacks scoped script permission", skill)
		}
		if strings.Contains(body, "https://oberth.ci/install.sh") {
			t.Fatalf("%s runs a website script", skill)
		}
	}
}

// Verify the actual downloader's TLS and redirect flags against an HTTPS
// release origin, in addition to the transport-independent adversarial cases.
func TestPluginFetchOverHTTPS(t *testing.T) {
	requireOpenSSL(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("curl is required for the HTTPS bootstrap test")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	binary := []byte("verified HTTPS fixture bytes\n")
	var sums strings.Builder
	for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "oberth-1.2.3.tgz"} {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(binary), name)
	}
	digest := sha256.Sum256([]byte(sums.String()))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oberth/latest/VERSION":
			_, _ = w.Write([]byte("v1.2.3\n"))
		case "/oberth/v1.2.3/SHA256SUMS":
			_, _ = w.Write([]byte(sums.String()))
		case "/oberth/v1.2.3/SHA256SUMS.sig":
			_, _ = w.Write(signature)
		case "/oberth/v1.2.3/oberth-linux-amd64", "/oberth/v1.2.3/oberth-linux-arm64", "/oberth/v1.2.3/oberth-darwin-amd64", "/oberth/v1.2.3/oberth-darwin-arm64":
			_, _ = w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pins"), 0700); err != nil {
		t.Fatal(err)
	}
	script := string(mustRead(t, pluginPath(t, "scripts", "fetch-oberth")))
	script = strings.ReplaceAll(script, "https://releases.oberth.ci/oberth", server.URL+"/oberth")
	script = strings.ReplaceAll(script, "036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31", fmt.Sprintf("%x", sha256.Sum256(public)))
	scriptPath := filepath.Join(root, "scripts", "fetch-oberth")
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pins", "release-cosign.pub"), public, 0600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", scriptPath)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"), "TMPDIR="+root, "CURL_CA_BUNDLE="+caPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch over HTTPS: %v %s", err, output)
	}
	if string(mustRead(t, filepath.Join(root, "home", ".local", "bin", "oberth-v1.2.3"))) != string(binary) {
		t.Fatal("HTTPS fetch installed different bytes")
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

func TestPluginInstallUsesVerifiedCLIAndSharedModes(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"bin/helm":          "#!/bin/sh\nexit 0\n",
		"bin/kubectl":       "#!/bin/sh\nexit 0\n",
		"bin/oberth":        "#!/bin/sh\nexit 93\n",
		".local/bin/oberth": "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for mode, want := range map[string]string{
		"plan":   "install\n--plan\n--json\n--context\nchosen cluster\n",
		"verify": "install\n--verify\n--json\n--context\nchosen cluster\n",
	} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", pluginPath(t, "scripts", "install"), mode, "--context", "chosen cluster")
		cmd.Env = []string{"HOME=" + root, "PATH=" + filepath.Join(root, "bin")}
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != want {
			t.Fatalf("%s did not delegate to the verified CLI: %v %q", mode, err, out)
		}
	}
	if err := os.Remove(filepath.Join(root, "bin", "helm")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", pluginPath(t, "scripts", "install"), "run", "--secrets-file", "recovery.txt")
	cmd.Env = []string{"HOME=" + root, "PATH=" + filepath.Join(root, "bin")}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "Install helm first") || strings.Contains(string(out), "--secrets-file") {
		t.Fatalf("missing prerequisite reached the installer: %v %q", err, out)
	}
}
