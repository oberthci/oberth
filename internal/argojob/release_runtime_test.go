package argojob

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const runtimeFixtureSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const runtimeFixtureTag = "v1.2.3"
const runtimeFixtureDate = "2026-09-27T00:00:00Z"

// Execute the real release entrypoints. Only external services/tools are fakes;
// the downloaded executable is a canary that records inherited authority.
type releaseRuntimeFixture struct {
	t                                                *testing.T
	root, candidate, scratch, receipt, script, trace string
	env                                              []string
}

func newReleaseRuntimeFixture(t *testing.T) *releaseRuntimeFixture {
	t.Helper()
	f := &releaseRuntimeFixture{t: t, root: t.TempDir()}
	var err error
	f.candidate, err = os.MkdirTemp("/tmp", "oberth-release-test-")
	if err != nil {
		t.Fatal(err)
	}
	f.scratch, err = os.MkdirTemp("/tmp", "oberth-release-runtime-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(f.candidate); _ = os.RemoveAll(f.scratch) })
	f.receipt = filepath.Join(f.root, "receipts", "receipt")
	f.trace = filepath.Join(f.root, "trace")
	f.script = filepath.Join(repositoryRoot(t), ".oberth", "release.sh")
	if override := os.Getenv("OBERTH_TEST_RELEASE_SCRIPT"); override != "" {
		f.script = override
	}
	pin, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".oberth", "pins", "release-cosign.pub"))
	if err != nil {
		t.Fatal(err)
	}
	f.write(".oberth/pins/release-cosign.pub", string(pin))
	f.write("secrets/cosign-secret/COSIGN_KEY", "TEST_PRIVATE_KEY_CANARY")
	f.write("secrets/cosign-secret/COSIGN_PASSWORD", "TEST_PASSWORD_CANARY")
	f.write("secrets/cosign-secret/COSIGN_PUB", string(pin))
	f.write("secrets/gar-image-key/GAR_SA_KEY", releaseCredentialJSON("source-image-delegator"))
	f.write("secrets/gar-chart-key/GAR_SA_KEY", releaseCredentialJSON("source-chart-delegator"))
	f.write("secrets/gar-reader-key/GAR_SA_KEY", releaseCredentialJSON("source-read-delegator"))
	f.write("r2-config", "fixture-only")
	f.write("objects/oberth/cosign.pub", string(pin))
	f.write("objects/oberth/index.yaml", "fixture-index")
	f.version(runtimeFixtureTag, string(pin), false)
	for _, file := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "SHA256SUMS", "SHA256SUMS.sigstore.json", "cosign.pub", "release.json", "release.json.sigstore.json"} {
		b, e := os.ReadFile(filepath.Join(f.root, "objects", "oberth", runtimeFixtureTag, file))
		if e != nil {
			t.Fatal(e)
		}
		f.absolute(filepath.Join(f.candidate, file), string(b), 0600)
	}
	chart := "fixture-chart"
	chartName := "oberth-1.2.3.tgz"
	f.absolute(filepath.Join(f.candidate, chartName), chart, 0600)
	f.absolute(filepath.Join(f.candidate, "chart-sha256.txt"), fmt.Sprintf("sha256:%x\n", sha256.Sum256([]byte(chart))), 0600)
	digest := "sha256:" + strings.Repeat("b", 64)
	f.absolute(filepath.Join(f.candidate, "server-image.txt"), "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@"+digest+"\n", 0600)
	f.absolute(filepath.Join(f.candidate, "chart-image.txt"), "europe-west4-docker.pkg.dev/skipopsmain/oberth-helm/oberth@"+digest+"\n", 0600)
	f.write("objects/oberth/"+chartName, chart)
	f.write("objects/oberth/"+chartName+".sigstore.json", "valid")
	f.tool("git", `case "$1" in rev-parse) if [ "$2" = --git-dir ]; then echo .git; else echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi;; show) echo 2026-09-27T00:00:00Z;; diff) :;; *) exit 90;; esac`)
	f.tool("curl", runtimeFakeCurl)
	f.tool("cosign", `printf 'cosign:%s:keyenv=%s\n' "$1" "${COSIGN_KEY:+present}" >>"$FIXTURE_TRACE"
if [ -n "${COSIGN_PASSWORD:-}" ]; then printf 'cosign-password:%s\n' "$1" >>"$FIXTURE_TRACE"; fi
case "$1" in
 public-key) cat "$FIXTURE_ROOT/.oberth/pins/release-cosign.pub";;
 verify-blob) while [ "$#" -gt 0 ]; do if [ "$1" = --bundle ]; then shift; grep -qx valid "$1" || exit 91; fi; shift; done;;
 verify) if [ -e "$FIXTURE_ROOT/require-signature" ] && [ ! -e "$FIXTURE_ROOT/registry-signed" ]; then exit 1; fi;;
 sign) touch "$FIXTURE_ROOT/registry-signed";;
 sign-blob) while [ "$#" -gt 0 ]; do if [ "$1" = --bundle ]; then shift; printf "valid\n" >"$1"; fi; shift; done;;
 *) exit 90;;
esac`)
	f.tool("trivy", ":")
	f.tool("oberth-release-image", ":")
	f.tool("oberth-release-support", `case "$1" in
 registry-token) /usr/bin/python3 -I -S -B - "$2" "$3" "$4" <<'PYTOKEN'
import json, os, stat, sys
try:
 def unique(pairs):
  result = {}
  for key, value in pairs:
   if key in result: raise ValueError()
   result[key] = value
  return result
 def reject(value): raise ValueError()
 fd = os.open(sys.argv[2], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
 with os.fdopen(fd, 'rb') as stream:
  if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode): raise ValueError()
  body = stream.read(65537)
 if len(body) > 65536: raise ValueError()
 key = json.loads(body, object_pairs_hook=unique, parse_constant=reject)
 principal = {'publish-images': 'source-image-delegator', 'publish-chart': 'source-chart-delegator', 'verify': 'source-read-delegator'}[sys.argv[1]]
 if key['type'] != 'service_account' or key['project_id'] != 'skipopsmain' or key['client_email'] != principal + '@skipopsmain.iam.gserviceaccount.com': raise ValueError()
 with open(sys.argv[3], 'x') as output: output.write('fixture-restricted-access-token')
except (OSError, ValueError, KeyError, TypeError): sys.exit(1)
PYTOKEN
;;
 registry-digest) printf 'sha256:%064d\n' 0 | tr 0 b;;
 chart-index-state) echo exact;;
 semver-compare) if [ "$2" = "$3" ]; then echo 0; elif [ "$2" = v1.2.4 ]; then echo 1; else echo -1; fi;;
 *) exit 90;;
esac`)
	f.tool("helm", `case "$1" in
 pull) while [ "$#" -gt 0 ]; do if [ "$1" = --destination ]; then shift; cp "$FIXTURE_ROOT/objects/oberth/oberth-1.2.3.tgz" "$1/oberth-1.2.3.tgz"; fi; shift; done;;
 template) cat "$FIXTURE_CANDIDATE/server-image.txt";;
 *) exit 90;;
esac`)
	return f
}
func (f *releaseRuntimeFixture) absolute(p, body string, mode os.FileMode) {
	f.t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		f.t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), mode); e != nil {
		f.t.Fatal(e)
	}
}
func (f *releaseRuntimeFixture) write(p, body string) {
	f.absolute(filepath.Join(f.root, p), body, 0600)
}
func (f *releaseRuntimeFixture) tool(name, body string) {
	guard := ""
	if name != "cosign" {
		guard = "if [ -n \"${COSIGN_PASSWORD:-}\" ]; then printf 'PASSWORD_LEAK:%s\\n' '" + name + "' >>\"$FIXTURE_TRACE\"; fi\n"
	}
	f.absolute(filepath.Join(f.root, "bin", name), "#!/bin/sh\nset -eu\n"+guard+body+"\n", 0700)
}
func (f *releaseRuntimeFixture) version(tag, key string, mutate bool) {
	var checks strings.Builder
	for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64"} {
		body := fmt.Sprintf("#!/bin/sh\nprintf 'EXEC:%%s\\n' \"${OBERTH_SECRETSTORE_DIR:+credentialed}\" >>\"$FIXTURE_TRACE\"\nprintf 'oberth %s commit=aaaaaaaaaaaa date=%s\\n'\n", tag, runtimeFixtureDate)
		if mutate {
			body += "printf '# mutated\\n' >>\"$0\"\n"
		}
		f.write("objects/oberth/"+tag+"/"+name, body)
		fmt.Fprintf(&checks, "%x  %s\n", sha256.Sum256([]byte(body)), name)
	}
	f.write("objects/oberth/"+tag+"/SHA256SUMS", checks.String())
	f.write("objects/oberth/"+tag+"/SHA256SUMS.sigstore.json", "valid")
	f.write("objects/oberth/"+tag+"/cosign.pub", key)
	serverRef := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:" + strings.Repeat("b", 64)
	f.write("objects/oberth/"+tag+"/release.json", fmt.Sprintf(`{"schemaVersion":1,"component":"oberth","version":"%s","source":{"repository":"github.com/oberthci/oberth","sha":"%s"},"images":{"server":{"ref":"%s"}}}`, tag, runtimeFixtureSHA, serverRef))
	f.write("objects/oberth/"+tag+"/release.json.sigstore.json", "valid")
}
func (f *releaseRuntimeFixture) run(action string, credentialed bool, arguments ...string) (string, error) {
	release := f.candidate
	if action == "verify-public-runtime" || action == "cloudflare-phase" {
		release = f.scratch
	}
	c := exec.Command("/bin/sh", append([]string{f.script, action}, arguments...)...)
	c.Dir = f.root
	c.Env = []string{"PATH=" + filepath.Join(f.root, "bin") + ":/usr/bin:/bin", "HOME=" + f.root, "OBERTH_TOOLS_DIR=" + f.root, "OBERTH_RELEASE_TAG=" + runtimeFixtureTag, "OBERTH_RELEASE_SHA=" + runtimeFixtureSHA, "OBERTH_RELEASE_DIR=" + release, "OBERTH_RUNTIME_RECEIPT=" + f.receipt, "OBERTH_RELEASE_ORIGIN=https://public.example.test", "OBERTH_CHART_REPOSITORY=https://public.example.test/oberth", "OBERTH_R2_ENDPOINT=https://r2.example.test", "OBERTH_R2_BUCKET=bucket", "OBERTH_R2_CURL_CONFIG=" + filepath.Join(f.root, "r2-config"), "FIXTURE_ROOT=" + f.root, "FIXTURE_TRACE=" + f.trace, "FIXTURE_CANDIDATE=" + f.candidate}
	if credentialed {
		c.Env = append(c.Env, "OBERTH_SECRETSTORE_DIR="+filepath.Join(f.root, "secrets"))
	}
	c.Env = append(c.Env, f.env...)
	out, err := c.CombinedOutput()
	return string(out), err
}
func (f *releaseRuntimeFixture) log() string { b, _ := os.ReadFile(f.trace); return string(b) }

const runtimeFakeCurl = `out= headers= code= method=GET data= url=
while [ "$#" -gt 0 ]; do
 case "$1" in
  --output) out=$2; shift 2;; --dump-header) headers=$2; shift 2;; --write-out) code=yes; shift 2;;
  --request) method=$2; shift 2;; --data-binary) data=${2#@}; shift 2;;
  --config|--header|--proto|--connect-timeout|--max-time|--max-redirs|--max-filesize) shift 2;;
  --*) shift;; *) url=$1; shift;;
 esac
done
case "$url" in
 "https://r2.example.test/bucket?"*"list-type=2"*)
  prefix=$(printf '%s' "$url" | sed -n 's/.*prefix=\([^&]*\).*/\1/p')
  listing='<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated>'
  if [ -n "$prefix" ] && [ -d "$FIXTURE_ROOT/objects/$prefix" ]; then
   for entry in "$FIXTURE_ROOT/objects/$prefix"*; do
    [ -f "$entry" ] || continue
    name=${entry#"$FIXTURE_ROOT/objects/"}
    listing="$listing<Contents><Key>$name</Key></Contents>"
   done
  fi
  listing="$listing</ListBucketResult>"
  [ -z "$out" ] || printf '%s' "$listing" >"$out"
  [ -z "$code" ] || printf '200'
  printf 'curl:LIST:%s\n' "$prefix" >>"$FIXTURE_TRACE"
  exit 0
  ;;
 https://public.example.test/*) key=${url#https://public.example.test/};;
 https://r2.example.test/bucket/*) key=${url#https://r2.example.test/bucket/};;
 *) exit 90;;
esac
file=$FIXTURE_ROOT/objects/$key
printf 'curl:%s:%s\n' "$method" "$key" >>"$FIXTURE_TRACE"
if [ "$method" = PUT ] && [ -e "$FIXTURE_ROOT/fail-next-put" ] && [ "$(cat "$FIXTURE_ROOT/fail-next-put")" = "$key" ]; then
 rm "$FIXTURE_ROOT/fail-next-put"; : >"$out"; printf 500; exit 0
fi
if [ "$method" = DELETE ]; then
 if [ -f "$file" ]; then rm -f "$file"; fi
 [ -z "$out" ] || : >"$out"
 [ -z "$code" ] || printf '204'
 exit 0
fi
if [ "$method" = PUT ]; then mkdir -p "$(dirname "$file")"; cp "$data" "$file"; fi
status=200
if [ -f "$file" ]; then cp "$file" "$out"; else status=404; : >"$out"; fi
if [ -n "$headers" ]; then printf 'ETag: "fixture-etag"\r\n' >"$headers"; fi
if [ -n "$code" ]; then printf '%s' "$status"; elif [ "$status" != 200 ]; then exit 22; fi`

func TestCredentialedReleaseVerificationNeverExecutesPayloadOrExportsKey(t *testing.T) {
	f := newReleaseRuntimeFixture(t)
	out, err := f.run("verify", true)
	if err != nil {
		t.Fatalf("verify: %v\n%s\n%s", err, out, f.log())
	}
	if strings.Contains(f.log(), "EXEC:") || strings.Contains(f.log(), "keyenv=present") {
		t.Fatalf("credentialed verification expanded authority:\n%s", f.log())
	}
}

func TestPublicRuntimeRejectsUnverifiedAndMutatedInputs(t *testing.T) {
	for _, variant := range []string{"success", "key", "signature", "checksum", "checksum-path", "wrong-version", "mutates-after-execution", "credentialed"} {
		t.Run(variant, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			switch variant {
			case "key":
				f.write("objects/oberth/v1.2.3/cosign.pub", "untrusted")
			case "signature":
				f.write("objects/oberth/v1.2.3/SHA256SUMS.sigstore.json", "bad")
			case "checksum":
				f.write("objects/oberth/v1.2.3/oberth-linux-amd64", "bad")
			case "checksum-path":
				p := filepath.Join(f.root, "objects/oberth/v1.2.3/SHA256SUMS")
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				f.absolute(p, strings.Replace(string(b), "oberth-linux-amd64", "../oberth-linux-amd64", 1), 0600)
			case "wrong-version":
				key, _ := os.ReadFile(filepath.Join(f.root, ".oberth/pins/release-cosign.pub"))
				f.version("v1.2.4", string(key), false)
				for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "SHA256SUMS"} {
					b, e := os.ReadFile(filepath.Join(f.root, "objects/oberth/v1.2.4", name))
					if e != nil {
						t.Fatal(e)
					}
					f.write("objects/oberth/v1.2.3/"+name, string(b))
				}
			case "mutates-after-execution":
				key, _ := os.ReadFile(filepath.Join(f.root, ".oberth/pins/release-cosign.pub"))
				f.version(runtimeFixtureTag, string(key), true)
			}
			out, err := f.run("verify-public-runtime", variant == "credentialed")
			if variant == "success" {
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				if !strings.Contains(f.log(), "EXEC:\n") {
					t.Fatal("native runtime was not executed")
				}
				if _, e := os.Stat(f.receipt); e != nil {
					t.Fatal(e)
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe runtime accepted")
			}
			if _, e := os.Stat(f.receipt); !os.IsNotExist(e) {
				t.Fatalf("failed verification left acceptance receipt: %v", e)
			}
			if variant != "mutates-after-execution" && variant != "wrong-version" && strings.Contains(f.log(), "EXEC:") {
				t.Fatalf("executed before validation: %s", f.log())
			}
		})
	}
}

func TestReleaseSignerUsesKeyFileWithoutExportingPrivateKey(t *testing.T) {
	f := newReleaseRuntimeFixture(t)
	f.write("require-signature", "yes")
	out, err := f.run("publish-images", true)
	if err != nil {
		t.Fatalf("publish: %v %s", err, out)
	}
	if !strings.Contains(f.log(), "cosign:sign:keyenv=\n") || strings.Contains(f.log(), "keyenv=present") || strings.Contains(f.log(), "PASSWORD_LEAK:") || !strings.Contains(f.log(), "cosign-password:sign") {
		t.Fatalf("signing key escaped file custody: %s", f.log())
	}
}

func TestFinalizeRequiresBoundReceiptAndNeverExecutesOtherPayload(t *testing.T) {
	for _, variant := range []string{"missing", "wrong-tag", "wrong-sha", "wrong-checksums", "extra-line", "symlink", "changed-artifact", "changed-authoritative", "own-tag", "superseded", "retry-partial"} {
		t.Run(variant, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			if variant != "missing" {
				out, err := f.run("verify-public-runtime", false)
				if err != nil {
					t.Fatalf("runtime: %v %s", err, out)
				}
			}
			b, _ := os.ReadFile(f.receipt)
			switch variant {
			case "wrong-tag":
				f.absolute(f.receipt, strings.Replace(string(b), "tag=v1.2.3", "tag=v1.2.4", 1), 0600)
			case "wrong-sha":
				f.absolute(f.receipt, strings.Replace(string(b), runtimeFixtureSHA, strings.Repeat("b", 40), 1), 0600)
			case "wrong-checksums":
				f.absolute(f.receipt, strings.Replace(string(b), "checksums=", "checksums=0", 1), 0600)
			case "extra-line":
				f.absolute(f.receipt, string(b)+"extra=true\n", 0600)
			case "symlink":
				f.write("receipt-copy", string(b))
				if e := os.Remove(f.receipt); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(filepath.Join(f.root, "receipt-copy"), f.receipt); e != nil {
					t.Fatal(e)
				}
			case "changed-artifact":
				f.absolute(filepath.Join(f.candidate, "cosign.pub"), "changed", 0600)
			case "changed-authoritative":
				key, _ := os.ReadFile(filepath.Join(f.root, ".oberth/pins/release-cosign.pub"))
				f.version(runtimeFixtureTag, string(key), true)
			case "superseded":
				f.write("objects/oberth/latest/VERSION", "v1.2.4\n")
			case "retry-partial":
				f.write("fail-next-put", "oberth/latest/oberth-linux-arm64")
			}
			_ = os.Remove(f.trace)
			out, err := f.run("finalize", true)
			switch variant {
			case "own-tag", "retry-partial":
				if variant == "retry-partial" {
					if err == nil {
						t.Fatal("failed alias write accepted")
					}
					out, err = f.run("finalize", true)
				}
				if err != nil {
					t.Fatalf("finalize: %v\n%s\n%s", err, out, f.log())
				}
				for _, name := range []string{"oberth-linux-amd64", "oberth-linux-arm64", "oberth-darwin-amd64", "oberth-darwin-arm64", "SHA256SUMS", "SHA256SUMS.sigstore.json", "cosign.pub", "release.json", "release.json.sigstore.json"} {
					got, e := os.ReadFile(filepath.Join(f.root, "objects/oberth/latest", name))
					if e != nil {
						t.Fatal(e)
					}
					want, e := os.ReadFile(filepath.Join(f.root, "objects/oberth", runtimeFixtureTag, name))
					if e != nil {
						t.Fatal(e)
					}
					if string(got) != string(want) {
						t.Fatalf("incomplete current aliases after success: %s", name)
					}
				}
			case "superseded":
				if err != nil || !strings.Contains(out, "superseded by v1.2.4") {
					t.Fatalf("superseded: %v %s", err, out)
				}
				if strings.Contains(f.log(), "curl:PUT:oberth/latest/") || strings.Contains(f.log(), "curl:GET:oberth/v1.2.4/") {
					t.Fatalf("receipt authorized another tag: %s", f.log())
				}
			default:
				if err == nil {
					t.Fatal("unverified finalize accepted")
				}
				if strings.Contains(f.log(), "curl:PUT:") {
					t.Fatalf("wrote before receipt acceptance: %s", f.log())
				}
			}
			if strings.Contains(f.log(), "EXEC:") {
				t.Fatalf("finalize executed downloaded bytes with credentials: %s", f.log())
			}
		})
	}
}
