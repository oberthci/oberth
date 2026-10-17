package installer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestNormalizeClientHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want, err string }{
		{in: "192.168.64.5", want: "192.168.64.5"},
		{in: " Lab-VM.Local. ", want: "lab-vm.local"},
		{in: "::1", want: "::1"},
		{in: "0.0.0.0", err: "listen address"},
		{in: "::", err: "listen address"},
		{in: "https://lab:30443", err: "neither an IP"},
		{in: "lab:30022", err: "neither an IP"},
	} {
		got, err := NormalizeClientHost(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("NormalizeClientHost(%q) error = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeClientHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestValidateClientHostAndAlsoUpstreams(t *testing.T) {
	t.Parallel()
	cfg := Config{ClientHost: "LAB.example", AlsoUpstreams: []string{"codeberg.org/acme", "ssh://git@gitlab.com/beta"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ClientHost != "lab.example" {
		t.Errorf("ClientHost = %q", cfg.ClientHost)
	}
	if strings.Join(cfg.AlsoUpstreams, " ") != "ssh://git@codeberg.org/acme ssh://git@gitlab.com/beta" {
		t.Errorf("AlsoUpstreams = %q", cfg.AlsoUpstreams)
	}
	kind := Config{CreateKind: true, ClientHost: "192.168.1.9"}
	if err := kind.Validate(); err == nil || !strings.Contains(err.Error(), "127.0.0.1 only") {
		t.Errorf("a kind cluster must refuse a non-loopback client host, got %v", err)
	}
	if err := (&Config{AlsoUpstreams: []string{"codeberg.org"}}).Validate(); err == nil {
		t.Error("an upstream without an organization must be refused")
	}
}

func TestClientHostOverridesTheAPIServerHost(t *testing.T) {
	t.Parallel()
	deps := Deps{RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"}}
	if got := sshHostFromServer(deps); got != "localhost" {
		t.Fatalf("loopback API host = %q, want localhost", got)
	}
	deps.ClientHost = "192.168.64.5"
	if got := sshHostFromServer(deps); got != "192.168.64.5" {
		t.Fatalf("client host = %q", got)
	}
	deps.KindClusterName = "oberth"
	if got := sshHostFromServer(deps); got != "localhost" {
		t.Fatalf("kind must stay on localhost, got %q", got)
	}
}

func TestAlsoUpstreamNameAvoidsNamesAndOrganizations(t *testing.T) {
	t.Parallel()
	used := map[string]bool{"codeberg": true}
	orgs := map[string]bool{"cloudtaser": true, "skipops": true}
	if got := alsoUpstreamName("ssh://git@github.com/oberthci", used, orgs); got != "github" {
		t.Errorf("got %q, want github", got)
	}
	if got := alsoUpstreamName("ssh://git@codeberg.org/skipops", used, orgs); got != "codeberg-skipops" {
		t.Errorf("got %q, want codeberg-skipops", got)
	}
	used["codeberg-skipops"] = true
	if got := alsoUpstreamName("ssh://git@codeberg.org/skipops", used, orgs); got != "codeberg-2" {
		t.Errorf("got %q, want codeberg-2", got)
	}
}

func TestNextStepsNameEveryUpstreamAndHowToAddMore(t *testing.T) {
	t.Parallel()
	lines := NextSteps(NextStepsInput{
		Host:      "192.168.64.5",
		Context:   "default",
		Namespace: "oberth",
		Upstreams: []NextStepsUpstream{
			{Name: "github", BaseURL: "ssh://git@github.com/oberthci", Connected: true},
			{Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser", Connected: false},
		},
		SSHAlias: true,
	})
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"git clone ssh://git@192.168.64.5:30022/github/oberthci/<repo>.git",
		"git clone git@oberth:github/oberthci/<repo>.git",
		"Finish codeberg (codeberg.org/cloudtaser)",
		"kubectl --context default exec -n oberth deploy/oberth -c oberth -- oberth upstream add --yes --no-wait codeberg ssh://git@codeberg.org/cloudtaser",
		"oberth upstream add --yes github ssh://git@github.com/<org>",
		"kubectl --context default exec -i -n oberth deploy/oberth -c oberth -- oberth uplink add - you@laptop < you@laptop.pub",
		"OBERTH_BASE_URL=https://192.168.64.5:30443",
		"oberth runs --ref my-branch",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("next steps miss %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "(once codeberg.org accepts the upstream key)") {
		t.Errorf("a pending upstream must say when its clone works:\n%s", text)
	}
	if strings.Contains(text, "works on this machine only") {
		t.Errorf("a non-localhost host needs no localhost note:\n%s", text)
	}
	local := strings.Join(NextSteps(NextStepsInput{Upstreams: []NextStepsUpstream{{Name: "github", BaseURL: "ssh://git@github.com/x", Connected: true}}}), "\n")
	if !strings.Contains(local, "localhost works on this machine only") || !strings.Contains(local, "ssh://git@localhost:30022/github/x/<repo>.git") {
		t.Errorf("localhost next steps:\n%s", local)
	}
}

func TestConnectAlsoUpstreamsRegistersEachWithTheSameKey(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	cfg := Config{AlsoUpstreams: []string{"ssh://git@codeberg.org/cloudtaser", "ssh://git@github.com/oberthci", "ssh://git@codeberg.org/skipops"}}
	tw := newTableWriter(&buf, false)
	got := connectAlsoUpstreams(context.Background(), cfg, deps, tw, []NextStepsUpstream{{Name: "github", BaseURL: "ssh://git@github.com/oberthci"}})
	tw.WriteFooter()

	if len(got) != 2 || got[0].Name != "codeberg" || got[1].Name != "codeberg-skipops" {
		t.Fatalf("connected = %+v", got)
	}
	if len(host.addArgv) != 2 {
		t.Fatalf("upstream add ran %d times, want 2 (the duplicate organization is skipped)", len(host.addArgv))
	}
	for _, argv := range host.addArgv {
		if slices.Contains(argv, "--dedicated-key") {
			t.Errorf("a further organization must reuse the upstream key: %q", argv)
		}
	}
	if !strings.Contains(buf.String(), "already there") {
		t.Errorf("the duplicate organization is not reported:\n%s", buf.String())
	}
}

// Review fix: an IPv6 client host is bracketed in every URL.
func TestNextStepsBracketIPv6(t *testing.T) {
	t.Parallel()
	text := strings.Join(NextSteps(NextStepsInput{Host: "fd00::5", Upstreams: []NextStepsUpstream{{Name: "github", BaseURL: "ssh://git@github.com/x", Connected: true}}}), "\n")
	for _, want := range []string{"ssh://git@[fd00::5]:30022/github/x/<repo>.git", "https://[fd00::5]:30443/runs", "OBERTH_BASE_URL=https://[fd00::5]:30443"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if got := readyURL(Deps{ClientHost: "::1"}); got != "https://[::1]:30443/runs" {
		t.Errorf("readyURL = %q", got)
	}
	if URLHost("192.168.1.2") != "192.168.1.2" || URLHost("lab") != "lab" {
		t.Error("only IPv6 literals are bracketed")
	}
}

// Review fix: a generated name always passes the server's upstream-name rule.
func TestAlsoUpstreamNameIsAValidUpstreamName(t *testing.T) {
	t.Parallel()
	name := alsoUpstreamName("ssh://git@codeberg.org/My_Org.Team", map[string]bool{"codeberg": true}, map[string]bool{})
	if name != "codeberg-my-org-team" {
		t.Fatalf("name = %q", name)
	}
	if got := alsoUpstreamName("ssh://git@sys.example/acme", map[string]bool{}, map[string]bool{}); got != "forge-sys" {
		t.Fatalf("reserved forge label gave %q", got)
	}
	long := alsoUpstreamName("ssh://git@codeberg.org/"+strings.Repeat("x", 80), map[string]bool{"codeberg": true}, map[string]bool{})
	if len(long) > 53 || strings.HasSuffix(long, "-") {
		t.Fatalf("long name %q", long)
	}
}

// Review fix: --also-upstream on a deployment that already has an upstream
// connects the organizations instead of dropping them.
func TestFinishInstallConnectsAlsoUpstreamsOnAConfiguredDeployment(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t, upstreamRows: []string{"github   ssh   ssh://git@github.com/oberthci   SHA256:x"}}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	cfg := Config{AlsoUpstreams: []string{"codeberg.org/cloudtaser"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := finishInstallTest(context.Background(), cfg, deps, &buf); err != nil {
		t.Fatal(err)
	}
	if len(host.addArgv) != 1 || !slices.Contains(host.addArgv[0], "codeberg") || !slices.Contains(host.addArgv[0], "ssh://git@codeberg.org/cloudtaser") {
		t.Fatalf("upstream add argv %q\n%s", host.addArgv, buf.String())
	}
}

// Review fix: when onboarding cannot run, the further organizations are
// named with the command that connects them.
func TestFinishInstallNamesAlsoUpstreamsWhenOnboardingCannotRun(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	cfg := Config{AlsoUpstreams: []string{"codeberg.org/cloudtaser"}}
	_ = cfg.Validate()
	_ = finishInstallTest(context.Background(), cfg, deps, &buf)
	if !strings.Contains(buf.String(), "upstream add --yes codeberg ssh://git@codeberg.org/cloudtaser") {
		t.Fatalf("output:\n%s", buf.String())
	}
}

func TestSaveTokenFileKeepsADifferentToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if _, err := SaveTokenFile([]byte("  \n")); err == nil {
		t.Fatal("an empty token must be refused")
	}
	path, err := SaveTokenFile([]byte("oberth_one"))
	if err != nil || path != filepath.Join(dir, "oberth", "token") {
		t.Fatalf("%q %v", path, err)
	}
	if _, err := SaveTokenFile([]byte("oberth_one\n")); err != nil {
		t.Fatal(err)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 0 {
		t.Fatalf("the same token must not create a backup: %v", backups)
	}
	if _, err := SaveTokenFile([]byte("oberth_two")); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if old, _ := os.ReadFile(backups[0]); string(old) != "oberth_one\n" {
		t.Errorf("backup holds %q", old)
	}
	if cur, _ := os.ReadFile(path); string(cur) != "oberth_two\n" {
		t.Errorf("token file holds %q", cur)
	}
}
