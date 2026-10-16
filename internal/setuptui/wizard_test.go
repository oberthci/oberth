package setuptui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/oberthci/oberth/internal/installer"
)

const (
	fakeRoot   = "hvs.fake-root-token-value"
	fakeUnseal = "fake-unseal-key-base64=="
	fakeBearer = "oberth_fake_bearer_value_0000"
)

// keyPendingInstall plays the tuxbox run of #002: everything up, the uplink
// minted, the forge has not accepted the generated upstream key.
func keyPendingInstall(_ int, deps installer.InstallDeps) error {
	credential := func(label, value string) {
		if deps.CredentialSink == nil { // plain mode: the installer's own box
			_, _ = deps.Output.Write([]byte(label + "  " + value + "\n"))
			return
		}
		deps.CredentialSink(label, value)
	}
	reportSteps(deps, "render chart", "apply namespace", "deploy oberth", "install openbao")
	credential("Root token", fakeRoot)
	credential("Unseal key", fakeUnseal)
	_, _ = deps.Output.Write([]byte("openbao initialized\nwarning: echo " + fakeRoot + "\n"))
	reportSteps(deps, "secretstore server", "secretstore release", "tls fingerprints", "rollout ready", "upstream discovery", "mint uplink")
	credential("Bearer token", fakeBearer)
	deps.StepProgressSink("upstream discovery", "pending")
	deps.StepProgressSink("audit genesis", "pending")
	return installer.ErrOnboardingPartial
}

func acknowledgeAll(h *harness) {
	h.t.Helper()
	c := h.w.install.ceremony
	for range c.entries {
		h.letter("r")
		h.letter("s")
	}
	h.enter() // Continue
}

// The whole first run on a k3s host without kind: every step answered on its
// own screen, one install, the ceremony, the finish screen with the pending
// forge authorization and its recovery commands (#047, #002).
func TestFirstRunEndToEnd(t *testing.T) {
	host := newTestHost(t)
	fake := stubInstaller(t, keyPendingInstall)
	kubectl := stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})

	h := newHarness(t, Options{BinaryVersion: "v0.17.28"})
	if v := h.view(); !strings.Contains(v, "Oberth setup — Main menu") || !strings.Contains(v, "1. Cluster") || !strings.Contains(v, "8. Install") {
		t.Fatalf("setup must start at the main menu:\n%s", v)
	}
	h.answerAllSteps()
	if v := h.view(); !strings.Contains(v, "Review (7 of 8)") || !strings.Contains(v, "All answers checked") {
		t.Fatalf("review must re-check the answers:\n%s", v)
	}
	h.enter() // Continue: install

	if fake.calls != 1 {
		t.Fatalf("installer ran %d times", fake.calls)
	}
	cfg := fake.cfgs[0]
	if cfg.ContextName != "default" || cfg.ForgeURL != "github.com/acme" || cfg.ClientAccess != "both" ||
		cfg.SSHPublicKeyPath != host.pubKey || !cfg.InstallSecretStore || !cfg.Yes || cfg.CreateKind {
		t.Fatalf("installer config not wired from the answers: %+v", cfg)
	}
	if strings.Join(cfg.TLSExtraIPs, ",") != "192.168.86.193" || strings.Join(cfg.TLSExtraDNSNames, ",") != "tuxbox" {
		t.Fatalf("TLS names must default to the probed node: %v %v", cfg.TLSExtraDNSNames, cfg.TLSExtraIPs)
	}

	if h.w.install.view != installCeremony {
		t.Fatalf("credentials must stop at the ceremony:\n%s", h.view())
	}
	for _, line := range h.w.install.logLines {
		if strings.Contains(line, fakeRoot) {
			t.Fatalf("a credential reached the log: %q", line)
		}
	}
	acknowledgeAll(h)

	f, ok := h.w.screen.(*finishScreen)
	if !ok {
		t.Fatalf("after the ceremony the finish screen must open, got %T", h.w.screen)
	}
	if h.w.state.Install.Status != installPendingForge {
		t.Fatalf("install status = %q", h.w.state.Install.Status)
	}
	view := strings.Join(strings.Fields(h.viewAll()), " ")
	for _, want := range []string{
		"has not accepted its upstream key",
		"oberth upstream add --yes --no-wait github ssh://git@github.com/acme",
		"https://github.com/settings/ssh/new",
		f.verify.fingerprint,
		"git clone ssh://git@localhost:30022/github/acme/<repo>.git",
		"Store the bearer token",
	} {
		if want == "" || !strings.Contains(view, want) {
			t.Errorf("finish screen missing %q:\n%s", want, view)
		}
	}
	// Opening the finish screen is read-only: it must not re-register.
	for _, c := range kubectl.commands {
		if strings.Contains(c, "upstream add") {
			t.Fatalf("finish screen re-registered without Verify: %s", c)
		}
	}

	// The state file holds the answers and the pending state, no secret.
	statePath, _ := setupStatePath()
	if statePath != filepath.Join(host.home, ".config", "oberth", "setup.json") {
		t.Fatalf("state path %s ignores XDG_CONFIG_HOME", statePath)
	}
	info, err := os.Stat(statePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("setup.json must exist with mode 0600: %v %v", info, err)
	}
	data, _ := os.ReadFile(statePath)
	for _, secret := range []string{fakeRoot, fakeUnseal, fakeBearer} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("setup.json contains a secret:\n%s", data)
		}
	}
	if !strings.Contains(string(data), `"organization": "acme"`) || !strings.Contains(string(data), installPendingForge) {
		t.Fatalf("setup.json misses answers or the pending state:\n%s", data)
	}

	// Exit: scripts must see the partial result (#003), with the commands.
	h.w.exit = exitMenu
	var out bytes.Buffer
	if err := h.w.finish(&out); !errors.Is(err, installer.ErrOnboardingPartial) {
		t.Fatalf("pending forge authorization must exit as partial, got %v", err)
	}
	if !strings.Contains(out.String(), "upstream add") {
		t.Fatalf("exit must print the recovery command:\n%s", out.String())
	}
}

// viewAll renders the current screen tall enough that nothing scrolls.
func (h *harness) viewAll() string {
	h.send(tea.WindowSizeMsg{Width: 80, Height: 400})
	defer h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	return h.view()
}

// Verify re-runs the registration; once the forge accepts the key and the
// server is Ready, the pending state is cleared and saved (#002).
func TestVerifyClearsPendingState(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, keyPendingInstall)
	kubectl := stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.enter()
	acknowledgeAll(h)

	kubectl.acceptAdd = true
	h.enter() // Verify has focus
	if h.w.state.Install.Status != installComplete {
		t.Fatalf("verify must complete the install, status %q\n%s", h.w.state.Install.Status, h.viewAll())
	}
	ran := strings.Join(kubectl.commands, "\n")
	if !strings.Contains(ran, "--context default exec -n oberth deploy/oberth -c oberth -- oberth upstream add --yes --no-wait github ssh://git@github.com/acme") {
		t.Fatalf("verify must re-run the registration against the chosen context:\n%s", ran)
	}
	if !strings.Contains(h.viewAll(), "installed and Ready") {
		t.Fatalf("finish screen must say Ready:\n%s", h.viewAll())
	}
	saved := newWizardState(Options{})
	if _, err := loadSetupState(saved); err != nil || saved.Install.Status != installComplete {
		t.Fatalf("the cleared state must be saved: %v %q", err, saved.Install.Status)
	}
}

// A rerun starts at the menu with every answer and the pending install, and
// opening Install goes straight to the finish screen with Verify (#047).
func TestRerunResumesAtMenu(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, keyPendingInstall)
	stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})
	first := newHarness(t, Options{})
	first.answerAllSteps()
	first.enter()
	acknowledgeAll(first)

	h := newHarness(t, Options{})
	view := h.view()
	for _, want := range []string{"✓ 1. Cluster", "✓ 4. Forge & upstream key", "github/acme", "waiting for forge authorization"} {
		if !strings.Contains(view, want) {
			t.Errorf("resumed menu missing %q:\n%s", want, view)
		}
	}
	if m := h.w.screen.(*menuScreen); m.cursor != int(stepInstall) {
		t.Fatalf("a pending install must put the cursor on Install, got %d", m.cursor)
	}
	h.enter()
	if _, ok := h.w.screen.(*finishScreen); !ok {
		t.Fatalf("Install on a pending install must open the finish screen, got %T", h.w.screen)
	}

	// --fresh ignores the saved answers.
	st := newWizardState(Options{Fresh: true})
	if st.ForgeOrg != "" || st.done[stepCluster] {
		t.Fatal("a fresh state must not carry answers")
	}
}

// Revisiting one step from the menu returns to the review, not through every
// later step again.
func TestRevisitedStepReturnsToReview(t *testing.T) {
	newTestHost(t)
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.esc() // review → menu
	if _, ok := h.w.screen.(*menuScreen); !ok {
		t.Fatalf("esc on review must open the menu, got %T", h.w.screen)
	}
	h.letter("3") // TLS
	h.enter()
	h.expectQuestion("tls.names")
	h.clear()
	h.typeText("oberth.example.com, 10.0.0.5")
	h.enter()
	if _, ok := h.w.screen.(*reviewScreen); !ok {
		t.Fatalf("finishing a revisited step must return to the review, got %T", h.w.screen)
	}
	if got := h.w.state.Config.TLSExtraDNSNames; len(got) != 1 || got[0] != "oberth.example.com" {
		t.Fatalf("TLS names = %v", got)
	}
	// Go Back on a step's first screen is the menu.
	h.esc()
	h.letter("1")
	h.enter()
	h.esc()
	if _, ok := h.w.screen.(*menuScreen); !ok {
		t.Fatalf("Go Back on a step's first screen must open the menu, got %T", h.w.screen)
	}
}

// An install failure explains the error, names the step to revisit, and
// offers Retry, Go Back, Show log and Abort (#047).
func TestInstallFailureDialog(t *testing.T) {
	newTestHost(t)
	fake := stubInstaller(t, func(call int, deps installer.InstallDeps) error {
		reportSteps(deps, "render chart", "apply namespace", "deploy oberth", "rollout ready",
			"install openbao", "secretstore server", "secretstore release", "tls fingerprints")
		_, _ = deps.Output.Write([]byte("importing the upstream key\n"))
		return errors.New("upstream add: ssh: this private key is passphrase protected")
	})
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.enter()

	view := h.viewAll()
	for _, want := range []string{"The install failed", "passphrase protected", "upstream discovery", "step 4, Forge & upstream key", "<Retry>", "<Go Back>", "<Show log>", "<Abort>"} {
		if !strings.Contains(view, want) {
			t.Errorf("failure dialog missing %q:\n%s", want, view)
		}
	}
	if h.w.state.Install.Status != installFailed || h.w.state.Install.Phase != "upstream discovery + deploy keys" {
		t.Fatalf("failure not recorded: %+v", h.w.state.Install)
	}

	h.enter() // Retry has focus
	if fake.calls != 2 {
		t.Fatalf("Retry must run the installer again, calls=%d", fake.calls)
	}
	if !strings.Contains(h.viewAll(), "Earlier attempts (1)") {
		t.Fatalf("earlier failures must stay visible (#014):\n%s", h.viewAll())
	}

	h.right() // Go Back
	h.enter()
	h.expectQuestion("forge.type")

	// Show log keeps every attempt.
	h.w.setScreen(h.w.install)
	h.letter("l")
	if log := h.viewAll(); !strings.Contains(log, "attempt 2") || !strings.Contains(log, "importing the upstream key") {
		t.Fatalf("log view must keep every attempt:\n%s", log)
	}
	h.esc()
	if h.w.showLog {
		t.Fatal("esc must close the log")
	}
}

func TestResponsibleStep(t *testing.T) {
	st := &WizardState{}
	cases := []struct {
		phase int
		err   string
		want  stepID
	}{
		{0, "load kubeconfig context \"x\": unreachable", stepCluster},
		{8, "--upstream-key-file: passphrase", stepForge},
		{4, "openbao did not become ready", stepStore},
		{9, "uplink add failed", stepUplink},
		{7, "something else", stepTLS},
		{2, "helm upgrade failed", stepCluster},
	}
	for _, c := range cases {
		if got := responsibleStep(c.phase, c.err, st); got != c.want {
			t.Errorf("responsibleStep(%d, %q) = %s, want %s", c.phase, c.err, got.name(), c.want.name())
		}
	}
}

// The ceremony acknowledges each credential on its own; copying one no
// longer discards all of them, copy does not reveal, and only the bearer
// token is described as kept as a digest (#010).
func TestCeremonyPerEntry(t *testing.T) {
	c := newCeremony()
	root, unseal, bearer := []byte(fakeRoot), []byte(fakeUnseal), []byte(fakeBearer)
	c.add("Root token", root)
	c.add("Unseal key", unseal)
	c.add("Bearer token", bearer)
	if !bytes.Equal(root, make([]byte, len(root))) {
		t.Fatal("add must zero the delivery buffer (S2)")
	}
	c.cursor = 0

	cmd, _ := c.update(tea.KeyPressMsg(tea.Key{Code: 'c', Text: "c"}))
	if cmd == nil || !c.entries[0].copied || c.entries[0].revealed {
		t.Fatal("c copies the selected entry without revealing it")
	}
	if _, leave := c.act(cerContinue); leave {
		t.Fatal("one copied entry must not let the ceremony end")
	}
	c.act(cerSaved)
	if !c.entries[0].saved || c.entries[0].value != nil || c.entries[1].saved {
		t.Fatal("saving one entry must zero that entry only")
	}
	c.cursor = 1
	c.act(cerSaved)
	if c.entries[1].saved || c.errMsg == "" {
		t.Fatal("an entry must be revealed or copied before it can be marked saved")
	}
	c.act(cerReveal)
	c.act(cerSaved)
	c.cursor = 2
	c.act(cerReveal)
	c.act(cerSaved)
	if _, leave := c.act(cerContinue); !leave || c.pending() {
		t.Fatal("with every entry saved the ceremony continues")
	}

	if !strings.Contains(credentialPurpose("Bearer token"), "digest") ||
		strings.Contains(credentialPurpose("Root token"), "digest") ||
		strings.Contains(credentialPurpose("Unseal key"), "digest") {
		t.Fatal("only the bearer token is kept as a digest")
	}
	w := &wizard{width: 80, height: 24}
	spec := c.frame(w)
	if !strings.Contains(spec.keys, "↑/↓") {
		t.Fatalf("the key line must teach selection: %q", spec.keys)
	}
}

// Ctrl+c is confirmed, and an abort zeros every secret held.
func TestCtrlCConfirmedAbortZerosSecrets(t *testing.T) {
	newTestHost(t)
	h := newHarness(t, Options{})
	h.w.install.ceremony.add("Bearer token", []byte(fakeBearer))
	held := h.w.install.ceremony.entries[0].value
	h.key(tea.Key{Code: 'c', Mod: tea.ModCtrl})
	if h.quit || !strings.Contains(h.view(), "ctrl+c again") {
		t.Fatal("the first ctrl+c only asks")
	}
	h.key(tea.Key{Code: 'c', Mod: tea.ModCtrl})
	if !h.quit || h.w.exit != exitAbort {
		t.Fatal("the second ctrl+c aborts")
	}
	if !bytes.Equal(held, make([]byte, len(held))) {
		t.Fatal("abort must zero held credentials")
	}
}

// The identity field edits at a cursor, and an error disappears as soon as
// the value changes (#006).
func TestIdentityEditing(t *testing.T) {
	newTestHost(t)
	t.Setenv("USER", "dr@valarian.com")
	if err := validateUplinkIdentityTUI(suggestedIdentity()); err != nil {
		t.Fatalf("the suggested identity %q must be valid: %v", suggestedIdentity(), err)
	}
	h := newHarness(t, Options{})
	h.w.openQuestion(questionsOf(h.w.questions, stepUplink)[0])
	h.clear()
	h.typeText("alice@lap@top")
	h.enter()
	if !strings.Contains(h.view(), "exactly one @") {
		t.Fatalf("invalid identity must be rejected on its screen:\n%s", h.view())
	}
	h.key(tea.Key{Code: tea.KeyLeft})
	h.key(tea.Key{Code: tea.KeyLeft})
	h.key(tea.Key{Code: tea.KeyLeft})
	h.key(tea.Key{Code: tea.KeyBackspace}) // the second @
	if strings.Contains(h.view(), "exactly one @") {
		t.Fatal("editing must clear the stale error")
	}
	h.home()
	h.letter("x")
	h.enter()
	if h.w.state.UplinkIdentity != "xalice@laptop" {
		t.Fatalf("identity = %q", h.w.state.UplinkIdentity)
	}
	h.expectQuestion("uplink.key")
}

// Answers are validated on their own screen.
func TestQuestionValidation(t *testing.T) {
	host := newTestHost(t)
	qs := buildQuestions()
	find := func(id string) *question {
		for _, q := range qs {
			if q.id == id {
				return q
			}
		}
		t.Fatalf("no question %s", id)
		return nil
	}
	st := newWizardState(Options{})
	reject := func(id string, answer ...string) {
		t.Helper()
		if err := find(id).save(st, answer); err == nil {
			t.Errorf("%s accepted %q", id, answer)
		}
	}
	reject("cluster.namespaces", "oberth", "oberth", "openbao")
	reject("cluster.namespaces", "Oberth", "a", "b")
	reject("cluster.target", "nonexistent")
	reject("cluster.target", clusterChoiceKind) // kind not installed
	reject("cluster.file", filepath.Join(host.home, "missing"))
	reject("tls.names", "not_a name!")
	reject("forge.org", "")
	reject("forge.org", "acme/repo")
	reject("uplink.identity", "no-at-sign")
	reject("uplink.keyfile", filepath.Join(host.home, ".ssh", "missing.pub"))
	reject("store.address", "http://vault.internal")
	reject("forge.keyfile", writeWorkKeyFixture(t, "secret"))

	if err := find("forge.keyfile").save(st, []string{writeWorkKeyFixture(t, "")}); err != nil {
		t.Fatalf("an unencrypted work key must be accepted: %v", err)
	}
	if err := find("tls.names").save(st, []string{"Oberth.Example.com., 192.0.2.1, 2001:db8::1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Config.TLSExtraDNSNames, ",") != "oberth.example.com" || len(st.Config.TLSExtraIPs) != 2 {
		t.Fatalf("names %v ips %v", st.Config.TLSExtraDNSNames, st.Config.TLSExtraIPs)
	}

	// The cluster probe runs before moving on: an unreachable cluster stays
	// on its screen.
	stubClusterProbe(t, errors.New("cluster unreachable: connection refused"))
	h := newHarness(t, Options{})
	h.enter()
	h.enter()
	h.expectQuestion("cluster.target")
	if !strings.Contains(h.view(), "connection refused") {
		t.Fatalf("probe error must show on the cluster screen:\n%s", h.view())
	}
}

// A kubeconfig file: its contexts are listed and the chosen one is probed
// with that file.
func TestKubeconfigFileChoice(t *testing.T) {
	host := newTestHost(t)
	calls := stubClusterProbe(t, nil)
	other := filepath.Join(host.home, "other.yaml")
	writeKubeconfig(t, other, "prod", "prod", "lab")
	h := newHarness(t, Options{})
	h.enter()
	for h.w.screen.(*questionScreen).opts[h.w.screen.(*questionScreen).cursor].value != clusterChoiceFile {
		h.down()
	}
	h.enter()
	h.expectQuestion("cluster.file")
	h.typeText(other)
	h.enter()
	h.expectQuestion("cluster.context")
	if v := h.view(); !strings.Contains(v, "prod") || !strings.Contains(v, "lab") {
		t.Fatalf("contexts of the file must be listed:\n%s", v)
	}
	h.down()
	h.enter()
	h.expectQuestion("cluster.namespaces")
	if last := (*calls)[len(*calls)-1]; last != "prod|"+other && last != "lab|"+other {
		t.Fatalf("probe must use the file: %s", last)
	}
	if h.w.state.Config.KubeconfigPath != other {
		t.Fatalf("kubeconfig path = %q", h.w.state.Config.KubeconfigPath)
	}
}

// Kind is offered only when kind and docker exist (#032), and then needs no
// kubeconfig or probe.
func TestKindOfferedOnlyWithTools(t *testing.T) {
	newTestHost(t)
	options := func() []option { return buildQuestions()[0].options(newWizardState(Options{})) }
	for _, o := range options() {
		if o.value == clusterChoiceKind {
			t.Fatal("kind offered without kind and docker")
		}
	}
	stubKindTools(t, true)
	found := false
	for _, o := range options() {
		found = found || o.value == clusterChoiceKind
	}
	if !found {
		t.Fatal("kind must be offered when kind and docker exist")
	}
	st := newWizardState(Options{})
	if err := buildQuestions()[0].save(st, []string{clusterChoiceKind}); err != nil || !st.Config.CreateKind || st.contextForCommands() != installer.KindContextName {
		t.Fatalf("kind choice: %v %+v", err, st.Config)
	}
	if !strings.Contains(BuildCommandLine(st), "--create-kind") || strings.Contains(BuildCommandLine(st), "--context") {
		t.Fatalf("kind command: %s", BuildCommandLine(st))
	}
}

// --dry-mode stops at the review, prints the command, persists nothing.
func TestDryModeStopsAtReview(t *testing.T) {
	newTestHost(t)
	fake := stubInstaller(t, func(int, installer.InstallDeps) error { return nil })
	h := newHarness(t, Options{DryMode: true})
	h.answerAllSteps()
	h.enter()
	if !h.quit || h.w.exit != exitDry || fake.calls != 0 {
		t.Fatal("dry mode must quit at the review without installing")
	}
	var out bytes.Buffer
	if err := h.w.finish(&out); err != nil || !strings.Contains(out.String(), "--organization='acme'") || !strings.Contains(out.String(), "--client-access=both") {
		t.Fatalf("dry-mode output: %v\n%s", err, out.String())
	}
	if path, _ := setupStatePath(); fileExists(path) {
		t.Fatal("dry mode must not write setup.json")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// At 80 columns every line fits and long commands wrap instead of being
// clipped (#014).
func TestNarrowTerminalWraps(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, keyPendingInstall)
	stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.enter()
	acknowledgeAll(h)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 300})
	view := h.w.render()
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 80 {
			t.Fatalf("line wider than 80 columns: %q", line)
		}
	}
	joined := strings.Join(strings.Fields(ansiStrip(view)), " ")
	if !strings.Contains(joined, "oberth upstream add --yes --no-wait github ssh://git@github.com/acme") {
		t.Fatalf("the recovery command must survive wrapping:\n%s", ansiStrip(view))
	}
	// At 24 rows the content scrolls rather than being cut off.
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(h.view(), "more") {
		t.Fatalf("long content must say it scrolls:\n%s", h.view())
	}
}

// The saved report carries per-phase status, versions, the clone URL and the
// recovery commands, under a new name every time, and never a secret (#012).
func TestReport(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, keyPendingInstall)
	stubKubectl(t, &fakeKubectl{pubKey: testPublicKey(t)})
	h := newHarness(t, Options{BinaryVersion: "v0.17.28"})
	h.answerAllSteps()
	h.enter()
	acknowledgeAll(h)

	h.right() // Save report
	h.enter()
	f := h.w.screen.(*finishScreen)
	if f.report == "" || !strings.Contains(h.view(), "Report saved to") {
		t.Fatalf("saving must show the path: %q %q", f.report, f.reportErr)
	}
	first := f.report
	second, err := writeReport(h.w)
	if err != nil || second == first {
		t.Fatalf("a second report must get its own file: %v %s", err, second)
	}
	info, _ := os.Stat(first)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode %v", info.Mode())
	}
	data, _ := os.ReadFile(first)
	text := string(data)
	for _, want := range []string{"oberth CLI: v0.17.28", "server image: ghcr.io/oberthci/oberth:v0.17.28", "done     render chart",
		"pending  upstream discovery", "upstream add --yes --no-wait github", "git clone ssh://git@localhost:30022/github/acme/<repo>.git",
		"Install log (secrets masked"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{fakeRoot, fakeUnseal, fakeBearer} {
		if strings.Contains(text, secret) {
			t.Fatalf("report contains a secret:\n%s", text)
		}
	}

	// A write failure is reported, not swallowed.
	dir := filepath.Dir(first)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Geteuid() != 0 {
		h.enter()
		if f.reportErr == "" || !strings.Contains(h.view(), "Report not saved") {
			t.Fatal("a failed save must be shown")
		}
	}
}

func TestBuildCommandLineYesRequiresConfirmedRemote(t *testing.T) {
	state := &WizardState{}
	if strings.Contains(BuildCommandLine(state), "--yes") {
		t.Fatal("--yes must not be recommended when no cluster probe succeeded")
	}
	state.ClusterInfo = clusterInfoMsg{context: "gke-prod", err: errors.New("unreachable")}
	if strings.Contains(BuildCommandLine(state), "--yes") {
		t.Fatal("--yes must not be recommended when the cluster probe failed")
	}
	state.ClusterInfo = clusterInfoMsg{context: "k3s-tuxbox", isLocal: true}
	if strings.Contains(BuildCommandLine(state), "--yes") {
		t.Fatal("--yes must not be recommended for a local cluster")
	}
	state.ClusterInfo = clusterInfoMsg{context: "gke-prod", isLocal: false}
	if !strings.Contains(BuildCommandLine(state), "--yes") {
		t.Fatal("--yes expected for a positively identified remote cluster")
	}
}

// The TLS step describes the certificate the store will issue (#007).
func TestTLSDescribesServerPKI(t *testing.T) {
	st := newWizardState(Options{})
	explain := buildQuestions()[8].explain(st)
	for _, want := range []string{"ECDSA P-256", "oberth.oberth.svc.cluster.local", "oberth-goproxy"} {
		if !strings.Contains(explain, want) {
			t.Errorf("tls explanation missing %q: %s", want, explain)
		}
	}
	st.setStoreMode(storeInstallDev)
	if !strings.Contains(buildQuestions()[8].explain(st), "self-signed") {
		t.Error("a dev store keeps the self-signed certificate")
	}
}

// Rows follow the installer's order, so a failure blames the first
// unfinished phase, and the map agrees with the rows.
func TestInstallPhasesFollowInstallerOrder(t *testing.T) {
	if len(phaseSteps) != len(installPhases) {
		t.Fatal("every phase needs a responsible step")
	}
	for name, i := range stepNameToIndex {
		if !strings.HasPrefix(installPhases[i], strings.Fields(name)[0]) {
			t.Errorf("%q maps to row %q", name, installPhases[i])
		}
	}
	if stepNameToIndex["upstream discovery"] > stepNameToIndex["mint uplink"] {
		t.Fatal("upstream must precede the uplink")
	}
}

// A partial result is a pending key only on the installer's key-pending path
// (#016).
func TestPartialIsNotAlwaysPendingKey(t *testing.T) {
	newTestHost(t)
	stubInstaller(t, func(_ int, deps installer.InstallDeps) error {
		reportSteps(deps, "render chart", "apply namespace", "deploy oberth", "rollout ready")
		deps.StepProgressSink("mint uplink", "pending")
		return installer.ErrOnboardingPartial
	})
	stubKubectl(t, &fakeKubectl{})
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.enter()
	if h.w.state.Install.Status != installPartial {
		t.Fatalf("an uplink failure is not a pending forge key: %q", h.w.state.Install.Status)
	}
}

// Setup refuses before the first screen when helm or kubectl is missing (#030).
func TestRunRefusesWithoutHostTools(t *testing.T) {
	previous := checkHostTools
	checkHostTools = func(string, func(string) (string, error)) error { return errors.New("kubectl not found in PATH") }
	t.Cleanup(func() { checkHostTools = previous })
	var output bytes.Buffer
	err := Run(context.Background(), Options{Plain: true}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "kubectl not found") || output.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}

// Tab reaches the button row on every screen, and <Go Back> there does what
// esc does (#047: identical keys everywhere).
func TestTabReachesButtons(t *testing.T) {
	newTestHost(t)
	h := newHarness(t, Options{})
	h.enter() // menu → cluster
	h.enter() // cluster → namespaces
	h.expectQuestion("cluster.namespaces")
	for i := 0; i < 3; i++ { // three fields, then the first button
		h.tab()
	}
	if b := h.w.screen.(*questionScreen).focus.button(); b != buttonBack {
		t.Fatalf("tab past the fields must reach <Go Back>, got %d", b)
	}
	h.enter()
	h.expectQuestion("cluster.target")
	h.tab()
	h.enter() // <Go Back> on the first screen of a step: the menu
	if _, ok := h.w.screen.(*menuScreen); !ok {
		t.Fatalf("got %T", h.w.screen)
	}
}

// Leaving through the menu after a failed install exits non-zero, so a
// wrapper script cannot mistake it for success (#003, review finding).
func TestFinishAfterFailedInstallReturnsError(t *testing.T) {
	newTestHost(t)
	h := newHarness(t, Options{})
	h.w.state.Install.Status = installFailed
	h.w.state.Install.Phase = "deploy oberth (digest-pinned)"
	var out bytes.Buffer
	err := h.w.finish(&out)
	if err == nil || !strings.Contains(err.Error(), "install failed at \"deploy oberth") {
		t.Fatalf("finish after a failed install = %v", err)
	}
}

// A resumed setup keeps what the cluster probe found, so the finish screen,
// Verify and the equivalent command name the real cluster rather than
// localhost, and keep --yes for a remote one (review finding).
func TestResumeKeepsClusterIdentity(t *testing.T) {
	var st WizardState
	st.Config.ContextName = "prod"
	st.ClusterInfo = clusterInfoMsg{context: "prod", server: "https://10.0.0.5:6443", isLocal: false, nodeName: "node-a", nodeIP: "10.0.0.5"}
	saved := st.toSaved()
	var resumed WizardState
	resumed.fromSaved(saved)
	got := resumed.ClusterInfo
	if got.server != "https://10.0.0.5:6443" || got.isLocal || got.nodeIP != "10.0.0.5" || got.nodeName != "node-a" || got.context != "prod" {
		t.Fatalf("resumed cluster info = %+v", got)
	}
}
