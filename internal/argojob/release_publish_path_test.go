package argojob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the production entrypoints and scanner shell. Only external services,
// compiler/scanner binaries and fixed container paths are fixture boundaries.
// In particular, never resolve Go or publisher tools through the fixture PATH.
func restrictedReleaseScript(t *testing.T, source, destination string, scanner bool) {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var prelude strings.Builder
	prelude.WriteString("#!/bin/sh\nset -eu\n")
	if !scanner {
		prelude.WriteString("test \"$PATH\" = /usr/bin:/bin\n")
	}
	names := []string{"git", "curl", "sleep"}
	if scanner {
		names = []string{"curl"}
	}
	for _, name := range names {
		tool := name
		if scanner {
			tool = "scan-curl"
		}
		fmt.Fprintf(&prelude, "%s() { \"$FIXTURE_ROOT/bin/%s\" \"$@\"; }\n", name, tool)
	}
	for _, name := range []string{"go", "grype", "cosign", "crane", "helm", "trivy", "apko", "node", "npm", "npx", "wrangler"} {
		fmt.Fprintf(&prelude, "%s() { printf 'forbidden bare tool: %s\\n' >&2; return 127; }\n", name, name)
	}
	body := strings.ReplaceAll(string(raw), "/work/src/scripts/release-wif-credential.py", filepath.Join(filepath.Dir(destination), "scripts", "release-wif-credential.py"))
	body = strings.ReplaceAll(body, "ssh-keyscan -t ", `"$FIXTURE_ROOT/bin/ssh-keyscan" -t `)
	body = strings.ReplaceAll(body, "ssh-keygen -lf ", `"$FIXTURE_ROOT/bin/ssh-keygen" -lf `)
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(prelude.String()+body), 0700); err != nil {
		t.Fatal(err)
	}
}

func newRestrictedReleaseFixture(t *testing.T) *releaseRuntimeFixture {
	t.Helper()
	f := newReleaseRuntimeFixture(t)
	original := f.script
	f.script = filepath.Join(f.root, "release.sh")
	restrictedReleaseScript(t, original, f.script, false)
	restrictedReleaseScript(t, filepath.Join(repositoryRoot(t), "hack", "scan-static-git.sh"), filepath.Join(f.root, "hack", "scan-static-git.sh"), true)
	f.tool("sleep", ":")
	f.tool("scan-curl", `out=
while [ "$#" -gt 0 ]; do
 case "$1" in --output) out=$2; shift 2;; *) shift;; esac
done
test -n "$out"
cp "$FIXTURE_ROOT/grype.tar.gz" "$out"`)
	grype := `#!/bin/sh
set -eu
printf 'grype:%s\n' "$(basename "$1")" >>"$FIXTURE_TRACE"
case "$1" in
 *static-git-control.cdx.json) printf '{"matches":["fixture-high"]}\n'; test "${FIXTURE_FAILURE:-}" = control-clean && exit 0; exit 2;;
 *static-git-arm64.cdx.json) test "${FIXTURE_FAILURE:-}" != arm64-scan || exit 41;;
esac
printf '{"matches":[]}\n'
`
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{Name: "grype", Mode: 0700, Size: int64(len(grype))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(grype)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	f.absolute(filepath.Join(f.root, "grype.tar.gz"), archive.String(), 0600)
	pins, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal/staticgit/build.env"))
	if err != nil {
		t.Fatal(err)
	}
	// This is a fixture archive with its own honest digest, not a qualification
	// claim about the published Grype bytes or vulnerability database.
	f.write("internal/staticgit/build.env", string(pins)+fmt.Sprintf("\nGRYPE_AMD64_SHA256=%x\nGRYPE_ARM64_SHA256=%x\n", sha256.Sum256(archive.Bytes()), sha256.Sum256(archive.Bytes())))
	f.tool("scanner-driver", `test "$#" -eq 5
"$1" "$4"`)
	f.tool("go", `case "$1" in
 build)
  printf 'go:build:%s\n' "$*" >>"$FIXTURE_TRACE"
  test "${FIXTURE_FAILURE:-}" != go-build || exit 42
  while [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; cp "$FIXTURE_ROOT/bin/scanner-driver" "$1"; exit 0; fi; shift; done
  exit 90;;
 run)
  shift; test "$1" = "$FIXTURE_ROOT/hack/static-git-scan-receipt"; shift
  test "$1" = --arch; arch=$2
  printf 'go:receipt:%s\n' "$arch" >>"$FIXTURE_TRACE"
  printf '{"fixture":true,"arch":"%s"}\n' "$arch";;
 *) exit 90;;
esac`)
	f.tool("oberth-release-image", `printf 'image:%s\n' "$1" >>"$FIXTURE_TRACE"
case "$1" in
 static-sbom) printf 'fixture SBOM\n' >"$3";;
 publish|verify|publish-kvm|verify-kvm)
  if [ "$6" = wif ]; then
   printf '%s\n' europe-west4-docker.pkg.dev | docker-credential-oberth-wif get >/dev/null
  else test "$(cat "$6")" = fixture-restricted-access-token; fi
  if [ "$1" = verify ]; then
   count=0; if [ -f "$FIXTURE_ROOT/verify-count" ]; then count=$(cat "$FIXTURE_ROOT/verify-count"); fi
   count=$((count + 1)); printf '%s\n' "$count" >"$FIXTURE_ROOT/verify-count"
   if [ "$count" = 2 ] && [ "${FIXTURE_FAILURE:-}" = final-verify ]; then exit 43; fi
  fi
  if [ "$1" = publish-kvm ]; then
   for name in runtime harness; do printf 'europe-west4-docker.pkg.dev/skipopsmain/oberth/kvm-%s@sha256:%064d\n' "$name" 0 >"$FIXTURE_CANDIDATE/kvm-$name-image.txt"; done
  fi;;
 *) exit 90;;
esac`)
	f.tool("trivy", `printf 'trivy:%s\n' "$1" >>"$FIXTURE_TRACE"
test "${FIXTURE_FAILURE:-}" != trivy`)
	f.write("require-signature", "true")
	f.tool("apko", `printf 'apko:%s\n' "$1" >>"$FIXTURE_TRACE"
for last do :; done
printf 'inert fixture image\n' >"$last"`)
	support, err := os.ReadFile(filepath.Join(f.root, "bin/oberth-release-support"))
	if err != nil {
		t.Fatal(err)
	}
	f.absolute(filepath.Join(f.root, "bin/oberth-release-support"), strings.Replace(string(support), "{'publish-images':", "{'publish-kvm-harness': 'source-image-delegator', 'publish-images':", 1), 0700)
	f.write("scripts/release-wif-credential.py", "import json,sys\nassert sys.argv[1:]==['get']\nassert sys.stdin.read().strip()=='europe-west4-docker.pkg.dev'\nprint(json.dumps({'Username':'oauth2accesstoken','Secret':'synthetic-wif-token'}))\n")
	f.env = append(f.env, "GO="+filepath.Join(f.root, "bin/go"))
	return f
}

func runRestrictedRelease(f *releaseRuntimeFixture, action string, credentialed bool, arguments ...string) (string, error) {
	release := f.candidate
	if action == "verify-public-runtime" {
		release = f.scratch
	}
	env := []string{"-i", "PATH=/usr/bin:/bin", "HOME=" + f.root,
		"OBERTH_TOOLS_DIR=" + f.root, "OBERTH_RELEASE_TAG=" + runtimeFixtureTag, "OBERTH_RELEASE_SHA=" + runtimeFixtureSHA,
		"OBERTH_RELEASE_DIR=" + release, "OBERTH_RUNTIME_RECEIPT=" + f.receipt,
		"OBERTH_RELEASE_ORIGIN=https://public.example.test", "OBERTH_CHART_REPOSITORY=https://public.example.test/oberth",
		"OBERTH_R2_ENDPOINT=https://r2.example.test", "OBERTH_R2_BUCKET=bucket", "OBERTH_R2_CURL_CONFIG=" + filepath.Join(f.root, "r2-config"),
		"FIXTURE_ROOT=" + f.root, "FIXTURE_TRACE=" + f.trace, "FIXTURE_CANDIDATE=" + f.candidate}
	if credentialed {
		env = append(env, "OBERTH_SECRETSTORE_DIR="+filepath.Join(f.root, "secrets"))
	}
	env = append(env, f.env...)
	env = append(env, "/bin/sh", f.script, action)
	env = append(env, arguments...)
	command := exec.CommandContext(f.t.Context(), "/usr/bin/env", env...)
	command.Dir = f.root
	command.Env = []string{"PATH=/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestCredentialedPublishImagesRestrictedPath(t *testing.T) {
	for _, mode := range []string{"legacy", "wif", "go-build", "control-clean", "arm64-scan", "trivy", "final-verify", "bare-go-regression", "bare-go-run-regression"} {
		t.Run(mode, func(t *testing.T) {
			f := newRestrictedReleaseFixture(t)
			if mode == "wif" {
				f.env = append(f.env, "OBERTH_RELEASE_WIF=1")
			} else {
				f.env = append(f.env, "FIXTURE_FAILURE="+mode)
			}
			if mode == "bare-go-regression" || mode == "bare-go-run-regression" {
				path := filepath.Join(f.root, "hack/scan-static-git.sh")
				source, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Reintroduce exactly the reviewed defect; the rehearsal must turn red.
				before, after := `"$go_tool" build`, "go build"
				if mode == "bare-go-run-regression" {
					before, after = `"$go_tool" run`, "go run"
				}
				f.absolute(path, strings.ReplaceAll(string(source), before, after), 0700)
			}
			output, err := runRestrictedRelease(f, "publish-images", true)
			trace := f.log()
			if strings.Contains(output+trace, "PRIVATE_CREDENTIAL_SENTINEL") || strings.Contains(trace, "PASSWORD_LEAK") {
				t.Fatal("fixture credential escaped")
			}
			if mode == "legacy" || mode == "wif" {
				if err != nil {
					t.Fatalf("restricted publish: %v\n%s\n%s", err, output, trace)
				}
				previous := -1
				for _, marker := range []string{"image:publish", "image:verify", "image:static-sbom", "go:build:", "grype:static-git-control.cdx.json", "grype:static-git-amd64.cdx.json", "go:receipt:amd64", "grype:static-git-arm64.cdx.json", "go:receipt:arm64", "trivy:image", "cosign:public-key", "cosign:sign:", "image:verify"} {
					next := strings.Index(trace[previous+1:], marker)
					if next < 0 {
						t.Fatalf("missing ordered phase %q:\n%s", marker, trace)
					}
					previous += next + 1
				}
				for _, arch := range []string{"amd64", "arm64"} {
					if _, err := os.Stat(filepath.Join(f.candidate, "static-git-"+arch+".security.json")); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err == nil {
					t.Fatalf("failed stage %s became a successful publication", mode)
				}
				if mode != "final-verify" && strings.Contains(trace, "cosign:sign:") {
					t.Fatalf("failed prerequisite reached signing:\n%s", trace)
				}
				if strings.HasPrefix(mode, "bare-go-") && !strings.Contains(output, "forbidden bare tool: go") {
					t.Fatalf("regression not caught by exact PATH: %s", output)
				}
			}
			for _, name := range []string{".gar-token", ".docker", ".helm"} {
				if _, err := os.Stat(filepath.Join(f.root, "secrets/.runtime", name)); !os.IsNotExist(err) {
					t.Fatalf("derived auth survived: %s", name)
				}
			}
		})
	}
}

func TestOtherCredentialedReleaseActionsRestrictedPath(t *testing.T) {
	for _, action := range []string{"sign-binaries", "publish-chart", "verify", "publish-public", "finalize", "publish-kvm-harness", "publish-homebrew"} {
		t.Run(action, func(t *testing.T) {
			f := newRestrictedReleaseFixture(t)
			if action == "verify" {
				f.write("registry-signed", "true")
			}
			if action == "publish-homebrew" {
				f.write("secrets/homebrew-tap-key/SSH_KEY", "SYNTHETIC_SSH_KEY")
				f.tool("ssh-keyscan", `printf 'fixture-public-host-key\n'`)
				f.tool("ssh-keygen", `cat >/dev/null; printf '256 SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU fixture\n'`)
				original, err := os.ReadFile(filepath.Join(f.root, "bin/git"))
				if err != nil {
					t.Fatal(err)
				}
				prelude := `case "$1" in
 clone)
  for last do :; done
  mkdir -p "$last/Formula"
  if [ -f "$FIXTURE_ROOT/tap-formula" ]; then cp "$FIXTURE_ROOT/tap-formula" "$last/Formula/oberth.rb"; else printf 'old fixture\n' >"$last/Formula/oberth.rb"; fi
  exit 0;;
 -C)
  dir=$2; shift 2
  while [ "$1" = -c ]; do shift 2; done
  case "$1" in diff) exit 1;; add|commit) exit 0;; push) cp "$dir/Formula/oberth.rb" "$FIXTURE_ROOT/tap-formula"; exit 0;; *) exit 90;; esac;;
 esac
 `
				f.absolute(filepath.Join(f.root, "bin/git"), "#!/bin/sh\nset -eu\n"+prelude+string(original), 0700)
			}
			helm := strings.Replace(immutableChartFixture, "*) exit 90;;", `template) cat "$FIXTURE_CANDIDATE/server-image.txt";;
*) exit 90;;`, 1)
			f.tool("helm", helm)
			f.absolute(filepath.Join(f.candidate, "oberth-1.2.3.tgz.sigstore.json"), "valid", 0600)
			if action == "finalize" {
				if output, err := runRestrictedRelease(f, "verify-public-runtime", false); err != nil {
					t.Fatalf("tokenless prerequisite: %v %s", err, output)
				}
			}
			if output, err := runRestrictedRelease(f, action, true); err != nil {
				t.Fatalf("%s restricted PATH: %v\n%s\n%s", action, err, output, f.log())
			}
		})
	}
}

func TestCredentialedWebsiteRestrictedPath(t *testing.T) {
	f := newWebsiteScriptFixture(t)
	f.installed() // credential-free staging, using its existing offline fixture.
	f.write("secrets/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN", []byte(websiteFixtureToken), 0400)
	original := f.script
	f.script = filepath.Join(f.root, "restricted-release.sh")
	restrictedReleaseScript(t, original, f.script, false)
	release, err := os.MkdirTemp("/tmp", "oberth-release-website-path-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(release) })
	args := []string{"-i", "PATH=/usr/bin:/bin", "HOME=" + f.root, "OBERTH_TOOLS_DIR=" + f.root,
		"OBERTH_RELEASE_TAG=" + runtimeFixtureTag, "OBERTH_RELEASE_SHA=" + runtimeFixtureSHA, "OBERTH_RELEASE_DIR=" + release,
		"OBERTH_WEBSITE_DIR=" + f.websiteDir, "OBERTH_WEBSITE_INPUTS=" + f.inputs, "OBERTH_SECRETSTORE_DIR=" + filepath.Join(f.root, "secrets"),
		"FIXTURE_ROOT=" + f.root, "FIXTURE_TRACE=" + f.trace, "FIXTURE_TOKEN=" + websiteFixtureToken,
		"/bin/sh", f.script, "cloudflare-phase", "publish-website"}
	command := exec.CommandContext(t.Context(), "/usr/bin/env", args...)
	command.Dir = f.root
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restricted website: %v\n%s", err, out)
	}
	trace := f.log()
	if !strings.Contains(trace, "deploy cwd=") || !strings.Contains(trace, "token=match") || strings.Contains(trace, websiteFixtureToken) {
		t.Fatalf("incorrect deployment boundary: %s", trace)
	}
}

func TestStaticScannerRejectsRelativeOrMissingGoBeforeDownload(t *testing.T) {
	for _, tool := range []string{"go", "/missing/go"} {
		t.Run(tool, func(t *testing.T) {
			f := newRestrictedReleaseFixture(t)
			c := exec.CommandContext(t.Context(), "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "GO="+tool, "FIXTURE_ROOT="+f.root, "/bin/sh", filepath.Join(f.root, "hack/scan-static-git.sh"), f.candidate, "amd64")
			c.Dir = f.root
			out, err := c.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "static Git scan:") {
				t.Fatalf("unsafe/missing Go accepted: %v %s", err, out)
			}
		})
	}
}
