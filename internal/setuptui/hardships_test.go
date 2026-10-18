package setuptui

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/mdnsdial"
)

func questionByID(t *testing.T, id string) *question {
	t.Helper()
	for _, q := range buildQuestions() {
		if q.id == id {
			return q
		}
	}
	t.Fatalf("no question %s", id)
	return nil
}

// writeKeyPair puts an unencrypted private key and its .pub into ~/.ssh.
func writeKeyPair(t *testing.T, home, name string) string {
	t.Helper()
	src := writeWorkKeyFixture(t, "")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(home, ".ssh", name)
	if err := os.WriteFile(private, data, 0o600); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	key, err := ssh.ParseRawPrivateKey(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pub := append([]byte(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))), []byte(" me@lab\n")...)
	if err := os.WriteFile(private+".pub", pub, 0o600); err != nil {
		t.Fatal(err)
	}
	return private
}

// #057: the key screen lists real key pairs from ~/.ssh, never an invented
// path, and finds a key created in another terminal on a rescan.
func TestUpstreamKeyPickerListsRealKeyPairs(t *testing.T) {
	host := newTestHost(t) // ~/.ssh has id_ed25519.pub without its private half
	st := newWizardState(Options{})
	st.ForgeAuth = forgeAuthExisting
	pick := questionByID(t, "forge.keypick")

	values := func() []string {
		var v []string
		for _, o := range pick.options(st) {
			v = append(v, o.value)
		}
		return v
	}
	if got := values(); strings.Join(got, ",") != upstreamKeyRescan+","+upstreamKeyOther {
		t.Fatalf("a .pub without its private key must not be offered: %v", got)
	}
	if !strings.Contains(pick.explain(st), "ssh-keygen -t ed25519") {
		t.Errorf("with no key pair the screen must say how to make one:\n%s", pick.explain(st))
	}

	private := writeKeyPair(t, host.home, "id_work")
	if err := pick.save(st, []string{upstreamKeyRescan}); !errors.Is(err, errRescan) {
		t.Fatalf("rescan = %v", err)
	}
	opts := pick.options(st)
	if opts[0].value != private || !strings.Contains(opts[0].detail, "me@lab") || !strings.Contains(opts[0].detail, "SHA256:") {
		t.Fatalf("the new key pair is not listed first with its fingerprint and comment: %+v", opts[0])
	}
	if err := pick.save(st, []string{private}); err != nil {
		t.Fatal(err)
	}
	if st.Config.UpstreamPrivateKeyPath != private {
		t.Fatalf("chosen key = %q", st.Config.UpstreamPrivateKeyPath)
	}
	if questionByID(t, "forge.keyfile").appliesTo(st) {
		t.Error("the path screen must only follow \"Another file…\"")
	}
	if err := pick.save(st, []string{upstreamKeyOther}); err != nil || !questionByID(t, "forge.keyfile").appliesTo(st) {
		t.Fatalf("\"Another file…\" must lead to the path screen: %v", err)
	}
}

// #057: a placeholder reads as an example whether or not its field has focus.
func TestPlaceholderNeverLooksTyped(t *testing.T) {
	in := newLineInput("")
	for _, focused := range []bool{false, true} {
		view := ansiStrip(in.view(40, focused, "~/.ssh/<private key file>"))
		if !strings.Contains(view, "e.g. ~/.ssh/<private key file>") {
			t.Errorf("focused=%v placeholder view %q", focused, view)
		}
	}
}

// #061: the TLS step offers the node's address, keeps kind on localhost, and
// the answer reaches the installer and the equivalent command.
func TestClientHostChoice(t *testing.T) {
	newTestHost(t)
	st := newWizardState(Options{})
	st.ClusterInfo = clusterInfoMsg{engine: "k3s", server: "https://127.0.0.1:6443", nodeName: "Lab-5be5", nodeIP: "192.168.64.5"}
	st.Config.TLSExtraDNSNames = []string{"lab.example"}
	q := questionByID(t, "tls.client")
	var values []string
	for _, o := range q.options(st) {
		values = append(values, o.value)
	}
	if strings.Join(values, ",") != "192.168.64.5,lab-5be5,lab.example,localhost" {
		t.Fatalf("options %v", values)
	}
	if q.selected(st) != "localhost" {
		t.Errorf("default must stay what the installer would advertise, got %q", q.selected(st))
	}
	if err := q.save(st, []string{"192.168.64.5"}); err != nil {
		t.Fatal(err)
	}
	if installConfig(st).ClientHost != "192.168.64.5" || !strings.Contains(BuildCommandLine(st), "--client-host='192.168.64.5'") {
		t.Fatalf("client host not wired: %q / %s", installConfig(st).ClientHost, BuildCommandLine(st))
	}
	if clientHost(st) != "192.168.64.5" {
		t.Errorf("finish screen host = %q", clientHost(st))
	}
	if err := q.save(st, []string{"10.9.9.9"}); err == nil {
		t.Error("an address that is not offered must be refused")
	}

	kind := newWizardState(Options{})
	kind.Config.CreateKind = true
	if opts := q.options(kind); len(opts) != 1 || opts[0].value != "localhost" {
		t.Errorf("kind must offer localhost only: %+v", opts)
	}
}

// #060: more organizations go to the installer as ssh:// URLs.
func TestMoreOrganizations(t *testing.T) {
	newTestHost(t)
	st := newWizardState(Options{})
	st.ForgeType, st.ForgeOrg = "github", "oberthci"
	q := questionByID(t, "forge.more")
	if err := q.save(st, []string{"codeberg.org/cloudtaser, ssh://git@codeberg.org/skipops, codeberg.org/cloudtaser"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Config.AlsoUpstreams, " "); got != "ssh://git@codeberg.org/cloudtaser ssh://git@codeberg.org/skipops" {
		t.Fatalf("AlsoUpstreams = %q", got)
	}
	if cmd := BuildCommandLine(st); !strings.Contains(cmd, "--also-upstream='codeberg.org/cloudtaser'") || !strings.Contains(cmd, "--also-upstream='codeberg.org/skipops'") {
		t.Errorf("equivalent command:\n%s", cmd)
	}
	if err := q.save(st, []string{"github.com/oberthci"}); err == nil {
		t.Error("the first organization must not be added twice")
	}
	if err := q.save(st, []string{"codeberg.org"}); err == nil {
		t.Error("a host without an organization must be refused")
	}
	if err := q.save(st, []string{""}); err != nil || len(st.Config.AlsoUpstreams) != 0 {
		t.Errorf("empty must clear the list: %v %v", err, st.Config.AlsoUpstreams)
	}
	ups := nextStepsUpstreams(st, nil)
	if len(ups) != 1 || ups[0].Name != "github" {
		t.Errorf("next steps upstreams %+v", ups)
	}
}

// #058: Verify reports every upstream's forge authentication, the ports on
// the client address and the token, and leaves a summary line.
func TestVerifyChecksAndSummary(t *testing.T) {
	newTestHost(t)
	stubKubectl(t, &fakeKubectl{registered: true, ready: true})
	dialPort = func(_ context.Context, address string) error {
		if strings.HasSuffix(address, ":30443") {
			return errors.New("connection refused")
		}
		return nil
	}
	st := newWizardState(Options{})
	st.ForgeType, st.ForgeOrg = "github", "acme"
	res := verifyInstall(context.Background(), *st, false)
	if len(res.upstreams) != 1 || !res.upstreams[0].ok() || !res.ready {
		t.Fatalf("result %+v", res)
	}
	if res.portsOK || !strings.Contains(res.portsText, "30443: connection refused") {
		t.Errorf("ports %v %q", res.portsOK, res.portsText)
	}
	read, _ := installer.TokenCommands()
	if strings.HasPrefix(read, "cat ") && (!res.tokenChecked || res.tokenOK) {
		t.Errorf("an empty token file must fail the check: %+v", res)
	}
	if s := res.summary(); !strings.Contains(s, "checks failed") {
		t.Errorf("summary %q", s)
	}

	rows := parseUpstreamCheck([]byte(upstreamCheckTable("codeberg", "ssh://git@codeberg.org/x", "ssh: unable to authenticate")))
	if len(rows) != 1 || rows[0].ok() || rows[0].auth != "ssh: unable to authenticate" || rows[0].url != "ssh://git@codeberg.org/x" {
		t.Errorf("parsed %+v", rows)
	}
}

// #059: the log view never wraps a line (tables stay intact), scrolls
// sideways, and the whole log is written to a file.
func TestLogViewAndFile(t *testing.T) {
	newTestHost(t)
	w := newWizard(Options{}, newWizardState(Options{}))
	w.width, w.height = 60, 24
	long := "│ Upstream identity │ ~/.ssh/a/very/long/path/that/does/not/fit/in/sixty/columns │ ✓ │"
	w.install.openLogFile()
	w.install.appendLog(long)
	w.install.closeLogFile()

	frame := w.logFrame()
	if len(frame.body) != 1 {
		t.Fatalf("a long line must stay one line, got %d", len(frame.body))
	}
	if !strings.Contains(frame.keys, "sideways") || !strings.Contains(frame.explain, "setup-log-") {
		t.Errorf("keys %q explain %q", frame.keys, frame.explain)
	}
	w.openLog()
	w.updateLog("right")
	if shifted := ansiStrip(w.logFrame().body[0]); strings.HasPrefix(shifted, "│ Upstream") {
		t.Errorf("right must scroll sideways: %q", shifted)
	}
	data, err := os.ReadFile(w.install.logPath)
	if err != nil || !strings.Contains(string(data), long) {
		t.Fatalf("log file %q: %v %q", w.install.logPath, err, data)
	}
	if info, err := os.Stat(w.install.logPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("log file mode: %v %v", info, err)
	}
}

// The real server's aligned output, with the fingerprint replaced.
func TestParseUpstreamCheckServerOutput(t *testing.T) {
	out := "NAME       KIND   URL                                 KEY NAME   KEY FINGERPRINT                                      AUTH\n" +
		"codeberg   ssh    ssh://git@codeberg.org/cloudtaser   -          SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA   ok\n" +
		"github     ssh    ssh://git@github.com/oberthci       -          SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA   ok\n"
	rows := parseUpstreamCheck([]byte(out))
	if len(rows) != 2 || rows[0].name != "codeberg" || rows[0].url != "ssh://git@codeberg.org/cloudtaser" || !rows[0].ok() || !rows[1].ok() {
		t.Fatalf("parsed %+v", rows)
	}
}

// Review fix: a further organization the forge has not accepted yet is not
// in the server's listing, but stays on the finish screen as pending.
func TestNextStepsKeepPendingAlsoUpstreams(t *testing.T) {
	st := newWizardState(Options{})
	st.ForgeType, st.ForgeOrg = "github", "oberthci"
	st.Config.AlsoUpstreams = []string{"ssh://git@codeberg.org/cloudtaser"}
	v := &verifyResult{upstream: true, upstreams: []upstreamCheck{{name: "github", url: "ssh://git@github.com/oberthci", auth: "ok"}}}
	ups := nextStepsUpstreams(st, v)
	if len(ups) != 2 || ups[1].Name != "codeberg" || ups[1].Connected {
		t.Fatalf("upstreams %+v", ups)
	}
	if !(upstreamCheck{auth: "-"}).unchecked() {
		t.Error("AUTH \"-\" is not a failure")
	}
}

// Review fix: answers saved before the key list existed resume on the typed
// path screen.
func TestResumeTypedUpstreamKeyPath(t *testing.T) {
	st := newWizardState(Options{})
	st.fromSaved(savedSetup{Version: setupStateFileVersion, ForgeAuth: forgeAuthExisting, UpstreamPrivateKey: "/home/x/.ssh/work"})
	if st.UpstreamKeyPick != upstreamKeyOther || !questionByID(t, "forge.keyfile").appliesTo(st) {
		t.Fatalf("pick %q", st.UpstreamKeyPick)
	}
}

// #062: Save to file writes the bearer token only when pressed, only for the
// bearer token, to the file the client files read, and discards it here.
func TestCeremonySaveTokenToFile(t *testing.T) {
	host := newTestHost(t)
	previous := tokenFileFallback
	tokenFileFallback = func() bool { return true }
	t.Cleanup(func() { tokenFileFallback = previous })

	c := newCeremony()
	c.add("Root token", []byte("s.root"))
	c.add("Bearer token", []byte("oberth_abc"))
	path := filepath.Join(host.home, ".config", "oberth", "token")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("nothing may be written before the operator asks")
	}

	// Save to file picks the bearer token; the root token is never filed.
	bearer := c.entries[1]
	c.act(cerFile)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "oberth_abc\n" {
		t.Fatalf("token file %q: %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	if bearer.filedAt != path {
		t.Errorf("entry after filing: %+v", bearer)
	}
	w := newWizard(Options{}, newWizardState(Options{}))
	w.width, w.height = 100, 30
	if view := ansiStrip(strings.Join(c.frame(w).body, "\n")); !strings.Contains(view, "saved to ~/.config/oberth/token (0600)") {
		t.Errorf("frame:\n%s", view)
	}

	tokenFileFallback = func() bool { return false }
	c2 := newCeremony()
	c2.add("Bearer token", []byte("oberth_def"))
	c2.act(cerFile)
	if c2.entries[0].filedAt != "" {
		t.Error("with a secret store the token stays the operator's to store")
	}
}

// #066: a client address that does not resolve is reported as a name
// problem, with the mDNS hint for a .local name, not as closed ports.
func TestVerifyReportsUnresolvedClientHost(t *testing.T) {
	newTestHost(t)
	stubKubectl(t, &fakeKubectl{registered: true, ready: true})
	previousDial := dialPort
	t.Cleanup(func() { dialPort = previousDial })
	for host, want := range map[string]string{
		"tuxbox.local":   "cannot resolve tuxbox.local (" + mdnsdial.Hint + ")",
		"ci.example.lan": "cannot resolve ci.example.lan: no such host; clients need it in DNS or /etc/hosts",
	} {
		dialPort = func(_ context.Context, address string) error {
			name, _, _ := net.SplitHostPort(address)
			return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}}
		}
		st := newWizardState(Options{})
		st.ForgeType, st.ForgeOrg = "github", "acme"
		st.Config.ClientHost = host
		res := verifyInstall(context.Background(), *st, false)
		if res.portsOK || res.portsText != want {
			t.Errorf("%s: ports %v %q, want %q", host, res.portsOK, res.portsText, want)
		}
	}
}

// #066: a .local name is offered after the node's address with a warning,
// and never preselected over it.
func TestClientHostLocalNameGivesWayToTheNodeAddress(t *testing.T) {
	newTestHost(t)
	st := newWizardState(Options{})
	st.ClusterInfo = clusterInfoMsg{engine: "k3s", server: "https://tuxbox.local:6443", nodeName: "tuxbox", nodeIP: "192.168.86.193"}
	st.Config.TLSExtraDNSNames = []string{"tuxbox.local", "ci.example"}
	q := questionByID(t, "tls.client")
	var values []string
	for _, o := range q.options(st) {
		values = append(values, o.value)
		if strings.HasSuffix(o.value, ".local") != strings.Contains(o.detail, "mDNS") {
			t.Errorf("option %q detail %q", o.value, o.detail)
		}
	}
	if strings.Join(values, ",") != "192.168.86.193,tuxbox,ci.example,tuxbox.local,localhost" {
		t.Fatalf("options %v", values)
	}
	if got := q.selected(st); got != "192.168.86.193" {
		t.Errorf("a .local API host must not be preselected over the node address, got %q", got)
	}
	if strings.Contains(q.explain(st), "mDNS") {
		t.Errorf("no warning while the node address is selected: %q", q.explain(st))
	}
	if err := q.save(st, []string{"tuxbox.local"}); err != nil {
		t.Fatal(err)
	}
	if q.selected(st) != "tuxbox.local" || !strings.Contains(q.explain(st), "cni0") {
		t.Errorf("an explicit .local answer stays and is warned about: %q %q", q.selected(st), q.explain(st))
	}
}
