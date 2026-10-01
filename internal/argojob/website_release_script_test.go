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

const websiteFixtureToken = "WEBSITE_TOKEN_CANARY_0123456789"

// The fake Node runtime records every invocation. npm-cli.js "installs"
// wrangler into the current directory; wrangler.js reports a version or
// records a deploy, including whether the token arrived through the
// environment (and never through argv).
const websiteFakeNode = `#!/bin/sh
set -eu
case "$1" in
*/npm-cli.js)
	printf 'npm cwd=%s userconfig=%s globalconfig=%s args=%s\n' "$PWD" "${npm_config_userconfig:-}" "${npm_config_globalconfig:-}" "$*" >>"$FIXTURE_TRACE"
	case " $* " in *" --offline "*) [ -d "${FIXTURE_REQUIRE_CACHE:-/nonexistent}" ] || exit 71;; esac
	mkdir -p node_modules/wrangler/bin
	printf 'fixture wrangler\n' >node_modules/wrangler/bin/wrangler.js
	;;
*/wrangler.js)
	shift
	case "$1" in
	--version) printf '%s\n' "${FIXTURE_WRANGLER_VERSION:-4.111.0}";;
	deploy)
		token=absent
		if [ "${CLOUDFLARE_API_TOKEN:-}" = "$FIXTURE_TOKEN" ]; then token=match; elif [ -n "${CLOUDFLARE_API_TOKEN:-}" ]; then token=mismatch; fi
		printf 'deploy cwd=%s token=%s account=%s home=%s logs=%s secretsdir=%s args=%s\n' "$PWD" "$token" "${CLOUDFLARE_ACCOUNT_ID:-}" "${HOME:-}" "${WRANGLER_LOG_PATH:-}" "${OBERTH_SECRETSTORE_DIR:-unset}" "$*" >>"$FIXTURE_TRACE"
		mkdir -p "$HOME/.config"
		;;
	*) exit 90;;
	esac
	;;
*) exit 90;;
esac
`

type websiteScriptFixture struct {
	t                               *testing.T
	root, websiteDir, inputs, trace string
	script, tarball                 string
}

func newWebsiteScriptFixture(t *testing.T) *websiteScriptFixture {
	t.Helper()
	f := &websiteScriptFixture{t: t, root: t.TempDir()}
	var err error
	if f.websiteDir, err = os.MkdirTemp("/tmp", "oberth-website-test-"); err != nil {
		t.Fatal(err)
	}
	if f.inputs, err = os.MkdirTemp("/tmp", "oberth-website-inputs-test-"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(f.websiteDir); _ = os.RemoveAll(f.inputs) })
	f.trace = filepath.Join(f.root, "trace")
	f.script = filepath.Join(repositoryRoot(t), ".oberth", "release.sh")
	repository := repositoryRoot(t)
	for _, name := range []string{"website/package.json", "website/package-lock.json", "website/wrangler.jsonc", "website/public/index.html", "website/public/setup-secretstore.sh"} {
		body, err := os.ReadFile(filepath.Join(repository, name))
		if err != nil {
			t.Fatal(err)
		}
		f.write(name, body, 0o600)
	}
	f.tarball = f.nodeTarball(websiteFakeNode)
	f.pin(f.tarball, websiteFakeNode)
	f.write("bin/git", []byte("#!/bin/sh\ncase \"$1\" in rev-parse) if [ \"$2\" = --git-dir ]; then echo .git; else echo "+runtimeFixtureSHA+"; fi;; diff) :;; config) :;; *) exit 90;; esac\n"), 0o700)
	return f
}

func (f *websiteScriptFixture) write(name string, body []byte, mode os.FileMode) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		f.t.Fatal(err)
	}
}

// nodeTarball builds a Node-shaped release tarball holding the fake runtime.
func (f *websiteScriptFixture) nodeTarball(node string) string {
	f.t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for _, entry := range []struct {
		name string
		body string
		mode int64
	}{
		{"node-v0.0.0-linux-x64/bin/node", node, 0o755},
		{"node-v0.0.0-linux-x64/lib/node_modules/npm/bin/npm-cli.js", "fixture npm\n", 0o644},
	} {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			f.t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.body)); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		f.t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		f.t.Fatal(err)
	}
	return buffer.String()
}

func (f *websiteScriptFixture) pin(tarball, node string) {
	f.write(".oberth/pins/node.sha256", fmt.Appendf(nil, "%x  /tmp/oberth-website-inputs/node.tar.gz\n", sha256.Sum256([]byte(tarball))), 0o600)
	f.write(".oberth/pins/node-bin.sha256", fmt.Appendf(nil, "%x  /tmp/oberth-website/node/bin/node\n", sha256.Sum256([]byte(node))), 0o600)
}

func (f *websiteScriptFixture) run(action string, credentialed bool, extra ...string) (string, error) {
	command := exec.Command("/bin/sh", f.script, action)
	command.Dir = f.root
	command.Env = []string{
		"PATH=" + filepath.Join(f.root, "bin") + ":/usr/bin:/bin", "HOME=" + f.root,
		"OBERTH_TOOLS_DIR=/tmp/oberth-tools", "OBERTH_RELEASE_TAG=" + runtimeFixtureTag, "OBERTH_RELEASE_SHA=" + runtimeFixtureSHA,
		"OBERTH_WEBSITE_DIR=" + f.websiteDir, "OBERTH_WEBSITE_INPUTS=" + f.inputs,
		"FIXTURE_TRACE=" + f.trace, "FIXTURE_TOKEN=" + websiteFixtureToken,
		"FIXTURE_REQUIRE_CACHE=" + filepath.Join(f.websiteDir, "npm-cache"),
	}
	if credentialed {
		command.Env = append(command.Env, "OBERTH_SECRETSTORE_DIR="+filepath.Join(f.root, "secrets"))
	}
	command.Env = append(command.Env, extra...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f *websiteScriptFixture) log() string {
	body, _ := os.ReadFile(f.trace)
	return string(body)
}

// installed brings the fixture to the state the two init containers leave:
// verified private copies, then an offline install.
func (f *websiteScriptFixture) installed() {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.websiteDir, "node.tar.gz"), []byte(f.tarball), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.websiteDir, "npm-cache", "_cacache"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if output, err := f.run("install-website-tools", false); err != nil {
		f.t.Fatalf("install-website-tools: %v\n%s\n%s", err, output, f.log())
	}
}

func TestReleaseScriptStagesWebsitePackagesWithoutScripts(t *testing.T) {
	f := newWebsiteScriptFixture(t)
	if err := os.WriteFile(filepath.Join(f.inputs, "node.tar.gz"), []byte(f.tarball), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("stage-website-packages", false)
	if err != nil {
		t.Fatalf("stage-website-packages: %v\n%s\n%s", err, output, f.log())
	}
	trace := f.log()
	for _, want := range []string{"--ignore-scripts", "--cache=" + f.inputs + "/npm-cache", "userconfig=" + f.websiteDir + "/npmrc-user globalconfig=" + f.websiteDir + "/npmrc-global", "--registry=https://registry.npmjs.org/", "cwd=" + f.websiteDir + "/app"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("staging npm call lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "--offline") || strings.Contains(trace, "deploy") || strings.Contains(trace, "wrangler") {
		t.Fatalf("staging must only populate the cache:\n%s", trace)
	}

	// A substituted tarball in the claim never reaches extraction or npm.
	f2 := newWebsiteScriptFixture(t)
	if err := os.WriteFile(filepath.Join(f2.inputs, "node.tar.gz"), []byte(f2.nodeTarball("#!/bin/sh\nexit 0\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := f2.run("stage-website-packages", false); err == nil || !strings.Contains(output, "differs from the reviewed pin") {
		t.Fatalf("substituted Node tarball was accepted: %v\n%s", err, output)
	}
	if strings.Contains(f2.log(), "npm") {
		t.Fatalf("npm ran from an unverified runtime:\n%s", f2.log())
	}

	// Credential material in the environment refuses the staging step.
	if output, err := f.run("stage-website-packages", true); err == nil || !strings.Contains(output, "refuses credential environment") {
		t.Fatalf("staging ran with a credential root: %v\n%s", err, output)
	}
}

func TestReleaseScriptInstallsWebsiteToolsOfflineAndChecksVersion(t *testing.T) {
	f := newWebsiteScriptFixture(t)
	f.installed()
	trace := f.log()
	if !strings.Contains(trace, "--offline --cache="+f.websiteDir+"/npm-cache") || !strings.Contains(trace, "--ignore-scripts") {
		t.Fatalf("install must be offline from the Pod-private verified cache:\n%s", trace)
	}

	// A wrangler that is not the lockfile's version fails closed.
	f2 := newWebsiteScriptFixture(t)
	if err := os.WriteFile(filepath.Join(f2.websiteDir, "node.tar.gz"), []byte(f2.tarball), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f2.websiteDir, "npm-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := f2.run("install-website-tools", false, "FIXTURE_WRANGLER_VERSION=4.0.0"); err == nil || !strings.Contains(output, "lockfile pins") {
		t.Fatalf("wrong wrangler version was accepted: %v\n%s", err, output)
	}

	// Without the verifier's private copies nothing is installed.
	f3 := newWebsiteScriptFixture(t)
	if output, err := f3.run("install-website-tools", false); err == nil || !strings.Contains(output, "verify-website-inputs must run first") {
		t.Fatalf("install ran without verified inputs: %v\n%s", err, output)
	}
}

func TestReleaseScriptPublishesWebsiteWithEnvironmentTokenOnly(t *testing.T) {
	f := newWebsiteScriptFixture(t)
	f.installed()
	f.write("secrets/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN", []byte(websiteFixtureToken+"\n"), 0o400)
	output, err := f.run("publish-website", true)
	if err != nil {
		t.Fatalf("publish-website: %v\n%s\n%s", err, output, f.log())
	}
	trace := f.log()
	deploy := ""
	for _, line := range strings.Split(trace, "\n") {
		if strings.HasPrefix(line, "deploy ") {
			deploy = line
		}
	}
	for _, want := range []string{
		"token=match", "account=0bc3ad9e8a1ef560fa5f1536d0696bc7", "secretsdir=unset",
		"cwd=" + f.websiteDir + "/site",
		"home=" + filepath.Join(f.root, "secrets", ".runtime", ".wrangler-home"),
		"args=deploy --config " + f.websiteDir + "/site/wrangler.jsonc --autoconfig=false --tag " + runtimeFixtureTag,
	} {
		if !strings.Contains(deploy, want) {
			t.Fatalf("deploy invocation lacks %q:\n%s", want, deploy)
		}
	}
	if strings.Contains(trace, websiteFixtureToken) || strings.Contains(output, websiteFixtureToken) {
		t.Fatal("the deploy token reached argv or output")
	}
	if _, err := os.Stat(filepath.Join(f.root, "secrets", ".runtime", ".wrangler-home")); !os.IsNotExist(err) {
		t.Fatalf("wrangler home on the secret mount survived the step: %v", err)
	}
	for _, name := range []string{"index.html", "setup-secretstore.sh"} {
		staged, _ := os.ReadFile(filepath.Join(f.websiteDir, "site", "public", name))
		source, _ := os.ReadFile(filepath.Join(f.root, "website", "public", name))
		if !bytes.Equal(staged, source) {
			t.Fatalf("staged %s differs from the source tree", name)
		}
	}
}

func TestReleaseScriptWebsitePublishFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*websiteScriptFixture)
	}{
		{"missing token", "CLOUDFLARE_API_TOKEN is missing", func(*websiteScriptFixture) {}},
		{"extra route", "not the reviewed oberth.ci Worker configuration", func(f *websiteScriptFixture) {
			f.write("secrets/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN", []byte(websiteFixtureToken), 0o400)
			config, _ := os.ReadFile(filepath.Join(f.root, "website", "wrangler.jsonc"))
			f.write("website/wrangler.jsonc", bytes.Replace(config, []byte(`"pattern": "www.oberth.ci"`), []byte(`"pattern": "watch.oberth.ci"`), 1), 0o600)
		}},
		{"script binding", "not the reviewed oberth.ci Worker configuration", func(f *websiteScriptFixture) {
			f.write("secrets/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN", []byte(websiteFixtureToken), 0o400)
			config, _ := os.ReadFile(filepath.Join(f.root, "website", "wrangler.jsonc"))
			f.write("website/wrangler.jsonc", bytes.Replace(config, []byte(`"name": "oberth-ci",`), []byte(`"name": "oberth-ci", "main": "worker.js",`), 1), 0o600)
		}},
		{"swapped runtime", "differs from the reviewed pin", func(f *websiteScriptFixture) {
			f.write("secrets/cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN", []byte(websiteFixtureToken), 0o400)
			path := filepath.Join(f.websiteDir, "node", "bin", "node")
			if err := os.WriteFile(path, []byte(websiteFakeNode+"# swapped after install\n"), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebsiteScriptFixture(t)
			f.installed()
			tc.mutate(f)
			output, err := f.run("publish-website", true)
			if err == nil || !strings.Contains(output, tc.want) {
				t.Fatalf("publish-website = %v, want refusal %q:\n%s", err, tc.want, output)
			}
			if strings.Contains(f.log(), "deploy ") {
				t.Fatalf("wrangler deploy ran despite the refusal:\n%s", f.log())
			}
		})
	}
}
