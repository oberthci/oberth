package installer

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type addRepoFixture struct {
	root, global, bin, trace string
	env                      []string
}

func newAddRepoFixture(t *testing.T, origin string) addRepoFixture {
	t.Helper()
	home := t.TempDir()
	f := addRepoFixture{root: filepath.Join(home, "repo with spaces"), global: filepath.Join(home, "gitconfig"), bin: filepath.Join(home, "bin"), trace: filepath.Join(home, "calls")}
	for _, dir := range []string{f.root, f.bin} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.global, []byte("[push]\n\tpushOption = global-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+f.global, "GIT_CONFIG_NOSYSTEM=1", "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CALL_TRACE="+f.trace)
	f.git(t, "init", "-b", "main")
	f.git(t, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(f.root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "add", "README.md")
	f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "fixture")
	return f
}
func (f addRepoFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", f.root}, args...)...)
	command.Env = f.env
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}
func (f addRepoFixture) call(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "sh", append([]string{pluginPath(t, "scripts", "add-repo")}, args...)...)
	command.Env = f.env
	out, err := command.CombinedOutput()
	return string(out), err
}
func (f addRepoFixture) fake(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAddRepoDetectsForgesWithoutDisclosingOriginCredentials(t *testing.T) {
	for _, test := range []struct{ origin, forge, org, repo, route, settings string }{
		{"git@github.com:acme/project.git", "github", "acme", "project", "acme", "https://github.com/acme/project/settings/keys"},
		{"ssh://git@gitlab.com/acme/subgroup/project.git", "gitlab", "acme/subgroup", "project", "subgroup", "https://gitlab.com/acme/subgroup/project/-/settings/repository#js-deploy-keys-settings"},
		{"https://codeberg.org/acme/project.git", "codeberg", "acme", "project", "acme", "https://codeberg.org/acme/project/settings/keys"},
	} {
		t.Run(test.forge, func(t *testing.T) {
			f := newAddRepoFixture(t, test.origin)
			out, err := f.call(t, "inspect", f.root)
			if err != nil {
				t.Fatalf("inspect failed: %v %s", err, out)
			}
			for _, want := range []string{"forge=" + test.forge, "organization=" + test.org, "repository=" + test.repo, "route_org=" + test.route, "key_settings=" + test.settings, "workflow=missing"} {
				if !strings.Contains(out, want) {
					t.Fatalf("inspection missing %s", want)
				}
			}
		})
	}
	f := newAddRepoFixture(t, "https://account:do-not-disclose@github.com/acme/project.git")
	out, err := f.call(t, "inspect", f.root)
	if err != nil || strings.Contains(out, "do-not-disclose") || strings.Contains(out, "account:") {
		t.Fatal("origin credentials disclosed or detection failed")
	}
	f.git(t, "remote", "set-url", "origin", "git@forge.example.test:acme/project.git")
	if _, err := f.call(t, "inspect", f.root); err == nil {
		t.Fatal("guessed an unknown forge")
	}
	if out, err := f.call(t, "inspect", f.root, "gitlab"); err != nil || !strings.Contains(out, "https://forge.example.test/acme/project/-/settings/repository") {
		t.Fatal("explicit custom forge failed")
	}
	f.git(t, "remote", "set-url", "origin", "ssh://git@forge.example.test:2222/acme/project.git")
	if out, err := f.call(t, "inspect", f.root, "gitlab"); err != nil || !strings.Contains(out, "upstream_url=ssh://git@forge.example.test:2222/acme") {
		t.Fatal("explicit forge SSH port was lost")
	}
	f.git(t, "remote", "set-url", "origin", "https://forge.example.test:8443/acme/project.git")
	if out, err := f.call(t, "inspect", f.root, "gitlab"); err != nil || !strings.Contains(out, "key_settings=https://forge.example.test:8443/acme/project/-/settings/repository") {
		t.Fatal("forge web settings port was lost")
	}
}

func TestAddRepoConfiguresOnlyTheRepositoryWithConsent(t *testing.T) {
	f := newAddRepoFixture(t, "git@github.com:acme/project.git")
	before, err := os.ReadFile(f.global)
	if err != nil {
		t.Fatal(err)
	}
	target := "git@oberth:github/acme/project.git"
	original := f.git(t, "rev-parse", "HEAD")
	f.git(t, "config", "--local", "--add", "push.pushOption", "other=value")
	if _, err := f.call(t, "configure", f.root, "remote", target, "no"); err == nil {
		t.Fatal("configured without consent")
	}
	for range 2 {
		if out, err := f.call(t, "configure", f.root, "remote", target, "--consent"); err != nil {
			t.Fatalf("configuration failed: %v %s", err, out)
		}
	}
	if got := f.git(t, "config", "--local", "--get-all", "push.pushOption"); got != "other=value\noberth.assist=1" {
		t.Fatal("did not preserve local options exactly once")
	}
	if f.git(t, "config", "--local", "--get", "remote.oberth.url") != target {
		t.Fatal("wrong Oberth remote")
	}
	different := "git@oberth:other/acme/project.git"
	if _, err := f.call(t, "configure", f.root, "remote", different, "--consent"); err == nil {
		t.Fatal("replaced an existing remote implicitly")
	}
	f.git(t, "config", "--local", "remote.oberth.pushurl", different)
	if _, err := f.call(t, "configure", f.root, "remote", target, "--consent"); err == nil {
		t.Fatal("ignored conflicting push URL")
	}
	if _, err := f.call(t, "configure", f.root, "replace-remote", target, "--consent"); err != nil {
		t.Fatal("explicit replacement failed")
	}
	if _, err := f.call(t, "configure", f.root, "push-url", target, "--consent"); err != nil {
		t.Fatal("push URL choice failed")
	}
	if f.git(t, "config", "--local", "--get", "remote.origin.url") != "git@github.com:acme/project.git" || f.git(t, "config", "--local", "--get", "remote.origin.pushurl") != target {
		t.Fatal("origin fetch/push choice incorrect")
	}
	after, err := os.ReadFile(f.global)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("global git configuration changed")
	}
	if original != f.git(t, "rev-parse", "HEAD") {
		t.Fatal("created a commit")
	}
}

func addRepoPublicKey(t *testing.T) string {
	t.Helper()
	key, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "upstream.pub")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAddRepoDeployKeyConsentAndManualFallback(t *testing.T) {
	for _, test := range []struct{ forge, origin, tool, endpoint, permission string }{
		{"github", "git@github.com:acme/project.git", "gh", "repos/acme/project/keys", "read_only=false"},
		{"gitlab", "git@gitlab.com:acme/subgroup/project.git", "glab", "projects/acme%2Fsubgroup%2Fproject/deploy_keys", "can_push=true"},
		{"codeberg", "git@codeberg.org:acme/project.git", "tea", "/repos/acme/project/keys", "read_only=false"},
	} {
		t.Run(test.forge, func(t *testing.T) {
			f := newAddRepoFixture(t, test.origin)
			public := addRepoPublicKey(t)
			f.fake(t, test.tool, `printf '%s\n' "$@" >> "$CALL_TRACE"
case " $* " in
 *" auth status "*) exit "${AUTH_EXIT:-0}" ;;
 *" /user "*) printf 'HTTP/2.0 %s status\n' "${AUTH_HTTP:-200}" >&2 ;;
 *) printf 'HTTP/2.0 %s status\n' "${WRITE_HTTP:-201}" >&2 ;;
esac
printf '%s\n' 'diagnostics must stay private' >&2
exit "${WRITE_EXIT:-0}"
`)
			if _, err := f.call(t, "deploy-key", f.root, public, "no"); err == nil {
				t.Fatal("forge mutation lacked consent")
			}
			if _, err := os.Stat(f.trace); !os.IsNotExist(err) {
				t.Fatal("invoked forge before consent")
			}
			out, err := f.call(t, "deploy-key", f.root, public, "--consent")
			if err != nil || !strings.Contains(out, "verify repository access through Oberth") || strings.Contains(out, "diagnostics must stay private") {
				t.Fatal("forge request failed or diagnostics disclosed")
			}
			calls, err := os.ReadFile(f.trace)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{test.endpoint, test.permission} {
				if !strings.Contains(string(calls), want) {
					t.Fatalf("forge arguments missing %s", want)
				}
			}
			f.env = append(f.env, "AUTH_EXIT=1", "AUTH_HTTP=401")
			out, err = f.call(t, "deploy-key", f.root, public, "--consent")
			if err == nil || !strings.Contains(out, "manual_key_settings=https://") {
				t.Fatal("missing manual fallback")
			}
			if !strings.Contains(out, "write access") {
				t.Fatal("manual fallback needs write access")
			}
			if !strings.Contains(err.Error(), "exit status 2") {
				t.Fatal("manual path must stop for the user")
			}
		})
	}
}

func TestAddRepoTeaRejectsHTTPFailureEvenWhenTheCLIExitsZero(t *testing.T) {
	f := newAddRepoFixture(t, "git@codeberg.org:acme/project.git")
	f.fake(t, "tea", `case " $* " in *" /user "*) echo 'HTTP/2.0 200 OK' >&2;; *) echo 'HTTP/2.0 403 Forbidden' >&2;; esac
exit 0
`)
	out, err := f.call(t, "deploy-key", f.root, addRepoPublicKey(t), "--consent")
	if err == nil || !strings.Contains(out, "https://codeberg.org/acme/project/settings/keys") {
		t.Fatal("HTTP failure was treated as success")
	}
}

func TestAddRepoWorkflowsAreNativeAndNeverCommittedOrOverwritten(t *testing.T) {
	for _, test := range []struct{ marker, language, task, prefix string }{{"go.mod", "go", "go test ./...", "golang:"}, {"package.json", "node", "npm ci", "node:"}, {"pyproject.toml", "python", "python -m compileall", "python:"}, {"", "generic", "test -d /work/src", "debian:"}} {
		t.Run(test.language, func(t *testing.T) {
			f := newAddRepoFixture(t, "git@github.com:acme/project.git")
			if test.marker != "" {
				if err := os.WriteFile(filepath.Join(f.root, test.marker), []byte("fixture\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			head := f.git(t, "rev-parse", "HEAD")
			preview, err := f.call(t, "workflow-preview", f.root)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(preview, test.task) || !strings.Contains(preview, "@sha256:") {
				t.Fatal("wrong language or unpinned image")
			}
			workflow, err := argoworkflow.Decode([]byte(preview))
			if err != nil {
				t.Fatal(err)
			}
			if err := argoworkflow.Admit(workflow, argoworkflow.Policy{RunnerImagePrefixes: []string{test.prefix}}); err != nil {
				t.Fatal(err)
			}
			if test.language == "node" || test.language == "python" {
				if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err == nil {
					t.Fatal("test must reflect the administrator image-policy gate")
				}
			}
			if _, err := f.call(t, "workflow-write", f.root, "no"); err == nil {
				t.Fatal("wrote without consent")
			}
			if _, err := os.Stat(filepath.Join(f.root, ".oberth")); !os.IsNotExist(err) {
				t.Fatal("preview or refused write changed repository")
			}
			if _, err := f.call(t, "workflow-write", f.root, "--consent"); err != nil {
				t.Fatal(err)
			}
			if head != f.git(t, "rev-parse", "HEAD") || f.git(t, "diff", "--cached", "--name-only") != "" {
				t.Fatal("staged or committed user files")
			}
			written, err := os.ReadFile(filepath.Join(f.root, ".oberth", "build.yaml"))
			if err != nil || string(written) != preview {
				t.Fatal("preview differs from write")
			}
			if _, err := f.call(t, "workflow-write", f.root, "--consent"); err == nil {
				t.Fatal("overwrote workflow")
			}
		})
	}
	f := newAddRepoFixture(t, "git@github.com:acme/project.git")
	if err := os.Symlink(t.TempDir(), filepath.Join(f.root, ".oberth")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, "workflow-write", f.root, "--consent"); err == nil {
		t.Fatal("followed .oberth symlink")
	}
}

func TestAddRepoClusterActionsKeepTheChosenContext(t *testing.T) {
	f := newAddRepoFixture(t, "git@github.com:acme/project.git")
	f.fake(t, "kubectl", `printf '%s\n' "$@" >> "$CALL_TRACE"`)
	if _, err := f.call(t, "prepare-upstream", "chosen", "namespace", "pod", "github", "ssh://git@github.com/acme", "project", "dedicated", "no"); err == nil {
		t.Fatal("upstream mutation lacked consent")
	}
	if _, err := f.call(t, "prepare-upstream", "chosen", "namespace", "pod", "github", "ssh://git@github.com/acme", "project", "dedicated", "--consent"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, "verify", "chosen", "namespace", "pod", "github/acme/project"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(f.trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--context=chosen", "--check-repo", "--dedicated-key", "repo\nverify\ngithub/acme/project"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("cluster invocation missing %s", want)
		}
	}
}

func TestAddRepoSmokeRequiresApprovedTargetCommittedWorkflowAndNewBranch(t *testing.T) {
	f := newAddRepoFixture(t, "git@github.com:acme/project.git")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "REAL_GIT="+realGit)
	f.fake(t, "git", `case " $* " in
 *" ls-remote "*) if [ "${EXISTING_BRANCH:-}" = yes ]; then echo 'head refs/heads/smoke'; fi; exit 0;;
 *" push "*) printf '%s\n' "$@" >> "$CALL_TRACE"; exit 0;;
esac
exec "$REAL_GIT" "$@"
`)
	target := "git@oberth:github/acme/project.git"
	if _, err := f.call(t, "configure", f.root, "remote", target, "--consent"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, f.git(t, "rev-parse", "HEAD"), "--consent"); err == nil {
		t.Fatal("pushed without committed workflow")
	}
	if _, err := f.call(t, "workflow-write", f.root, "--consent"); err != nil {
		t.Fatal(err)
	}
	f.git(t, "add", ".oberth/build.yaml")
	f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "approved fixture workflow")
	approvedCommit := f.git(t, "rev-parse", "HEAD")
	for _, args := range [][]string{{"smoke", f.root, "oberth", "smoke", target, approvedCommit, "no"}, {"smoke", f.root, "oberth", "smoke", "git@oberth:wrong/acme/project.git", approvedCommit, "--consent"}, {"smoke", f.root, "oberth", "../bad", target, approvedCommit, "--consent"}} {
		if _, err := f.call(t, args...); err == nil {
			t.Fatal("unsafe smoke push accepted")
		}
	}
	f.env = append(f.env, "EXISTING_BRANCH=yes")
	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, f.git(t, "rev-parse", "HEAD"), "--consent"); err == nil {
		t.Fatal("overwrote existing branch")
	}
	f.env = append(f.env, "EXISTING_BRANCH=no")
	if err := os.WriteFile(filepath.Join(f.root, "README.md"), []byte("unapproved change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, f.git(t, "rev-parse", "HEAD"), "--consent"); err == nil {
		t.Fatal("pushed dirty tracked files")
	}
	f.git(t, "checkout", "--", "README.md")
	f.git(t, "config", "--local", "--add", "remote.oberth.pushurl", target)
	f.git(t, "config", "--local", "--add", "remote.oberth.pushurl", "git@elsewhere:acme/project.git")
	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, approvedCommit, "--consent"); err == nil {
		t.Fatal("accepted multiple push destinations")
	}
	f.git(t, "config", "--local", "--unset-all", "remote.oberth.pushurl")
	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, strings.Repeat("f", 40), "--consent"); err == nil {
		t.Fatal("accepted an unapproved HEAD")
	}

	if _, err := f.call(t, "smoke", f.root, "oberth", "smoke", target, approvedCommit, "--consent"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(f.trace)
	if err != nil || !strings.Contains(string(calls), approvedCommit+":refs/heads/smoke") || !strings.Contains(string(calls), "\n"+target+"\n") || !strings.Contains(string(calls), "--force-with-lease=refs/heads/smoke:") || strings.Contains(string(calls), "\n--force\n") {
		t.Fatal("wrong smoke push")
	}
}

func TestAddRepoReusesThePluginCLIOutsidePATH(t *testing.T) {
	f := newAddRepoFixture(t, "git@github.com:acme/project.git")
	for _, tool := range []string{"git", "kubectl", "helm", "ssh", "ssh-keygen"} {
		f.fake(t, tool, "exit 0\n")
	}
	localBin := filepath.Join(filepath.Dir(f.root), ".local", "bin")
	if err := os.MkdirAll(localBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBin, "oberth"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CALL_TRACE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "PATH="+f.bin)
	if out, err := f.call(t, "prerequisites"); err != nil {
		t.Fatalf("plugin CLI fallback failed: %v %s", err, out)
	}
	if out, err := f.call(t, "validate", f.root); err != nil {
		t.Fatalf("validation fallback failed: %v %s", err, out)
	}
	calls, err := os.ReadFile(f.trace)
	if err != nil || !strings.Contains(string(calls), "validate") {
		t.Fatal("the plugin CLI was not used")
	}
}

func TestAddRepoDeployKeyRejectsTrailingMaterialAndStripsComments(t *testing.T) {
	for _, forge := range []struct{ host, tool string }{{"github.com", "gh"}, {"gitlab.com", "glab"}, {"codeberg.org", "tea"}} {
		t.Run(forge.tool, func(t *testing.T) {
			f := newAddRepoFixture(t, "git@"+forge.host+":acme/project.git")
			public := addRepoPublicKey(t)
			original, err := os.ReadFile(public)
			if err != nil {
				t.Fatal(err)
			}
			f.fake(t, forge.tool, `for arg do
 case "$arg" in
 key=@*) cat "${arg#key=@}" > "$CALL_TRACE";;
 key=*) printf '%s\n' "${arg#key=}" > "$CALL_TRACE";;
 esac
done
printf 'HTTP/2.0 201 Created\n' >&2
`)
			if err := os.WriteFile(public, append(append([]byte(nil), original...), []byte("-----BEGIN PRIVATE KEY-----\nprivate material\n-----END PRIVATE KEY-----\n")...), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.call(t, "deploy-key", f.root, public, "--consent"); err == nil {
				t.Fatal("uploaded trailing private material")
			}
			if _, err := os.Stat(f.trace); !os.IsNotExist(err) {
				t.Fatal("forge received an invalid public input")
			}
			commented := append(append([]byte(nil), bytes.TrimSpace(original)...), []byte(" private-comment-must-stay-local\n")...)
			if err := os.WriteFile(public, commented, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.call(t, "deploy-key", f.root, public, "--consent"); err != nil {
				t.Fatal("canonical public upload failed")
			}
			uploaded, err := os.ReadFile(f.trace)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(uploaded, original) {
				t.Fatal("forge received data outside the canonical public key")
			}
		})
	}
}
