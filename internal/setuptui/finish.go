package setuptui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/installer"
)

// ---------- verification against the installed server (#002) ----------

// runKubectl runs one kubectl command; tests replace it.
var runKubectl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "kubectl", args...).CombinedOutput() //nolint:gosec // G204: fixed binary; arguments are built by the wizard
}

// kubectlTarget is the --kubeconfig/--context pair the install targeted.
func kubectlTarget(st *WizardState) []string {
	var args []string
	if st.Config.KubeconfigPath != "" {
		args = append(args, "--kubeconfig", st.Config.KubeconfigPath)
	}
	if c := st.contextForCommands(); c != "" {
		args = append(args, "--context", c)
	}
	return args
}

func podExec(st *WizardState, command ...string) []string {
	args := append(kubectlTarget(st), "exec", "-n", st.namespace(), "deploy/oberth", "-c", "oberth", "--")
	return append(args, command...)
}

// kubectlLine renders a kubectl argv as a copyable command line.
func kubectlLine(args []string) string {
	parts := []string{"kubectl"}
	for _, a := range args {
		if strings.ContainsAny(a, " '\"$*?{}()|&;<>") {
			a = shellQuote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

func upstreamAddArgs(st *WizardState) []string {
	return podExec(st, "oberth", "upstream", "add", "--yes", "--no-wait", st.ForgeType,
		"ssh://git@"+forgeUpstreamURL(st.ForgeType, st.ForgeOrg))
}

const upstreamPublicKeyPath = "/run/oberth-identities/upstream/id_ed25519.pub"

// recoveryCommands are the exact commands that finish a pending forge
// authorization by hand (#002, #012).
func recoveryCommands(st *WizardState) []string {
	var lines []string
	if st.ForgeAuth != forgeAuthExisting {
		lines = append(lines,
			"1. Show the upstream public key:  "+kubectlLine(podExec(st, "cat", upstreamPublicKeyPath)),
			"2. Add it at "+forgeKeyDestination(st.ForgeType, st.ForgeOrg))
	} else {
		lines = append(lines, "1. Make sure the work key "+displayHome(st.Config.UpstreamPrivateKeyPath)+" is registered at "+forgeKeyDestination(st.ForgeType, st.ForgeOrg),
			"2. (nothing to copy: the key you chose is already in OpenBao)")
	}
	return append(lines, "3. Register the upstream:  "+kubectlLine(upstreamAddArgs(st)))
}

type fileCheck struct {
	path string
	ok   bool
}

// verifyResult is what one Verify run found.
type verifyResult struct {
	at          time.Time
	upstream    bool
	upstreams   []upstreamCheck
	ready       bool
	readyText   string
	image       string
	publicKey   string
	fingerprint string
	register    string // output of a failed re-registration, trimmed
	err         string
	// ports reports whether SSH and HTTPS answer on the client address;
	// portsText says where, or what failed.
	portsChecked bool
	portsOK      bool
	portsText    string
	// token reports whether the owner-only token file the client files read
	// holds a token; tokenChecked is false when a secret store keeps it.
	tokenChecked bool
	tokenOK      bool
	tokenText    string
}

// upstreamCheck is one row of `oberth upstream list --check`.
type upstreamCheck struct {
	name, url, auth string
}

func (u upstreamCheck) ok() bool { return u.auth == "ok" }

// unchecked is a row the server did not authenticate: no AUTH column (an
// older server) or "-" (a kind of upstream without a key check).
func (u upstreamCheck) unchecked() bool { return u.auth == "" || u.auth == "-" }

// orgOf is the organization (last path segment) of an upstream URL.
func orgOf(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	return trimmed[strings.LastIndex(trimmed, "/")+1:]
}

// checks counts what Verify checked and how much of it passed.
func (v *verifyResult) checks() (passed, total int) {
	count := func(ok bool) {
		total++
		if ok {
			passed++
		}
	}
	count(v.ready)
	if len(v.upstreams) == 0 {
		count(v.upstream)
	}
	for _, u := range v.upstreams {
		count(u.ok() || u.unchecked())
	}
	if v.portsChecked {
		count(v.portsOK)
	}
	if v.tokenChecked {
		count(v.tokenOK)
	}
	return passed, total
}

// summary is the one line Verify leaves on screen until the next action
// (#058).
func (v *verifyResult) summary() string {
	passed, total := v.checks()
	if passed == total {
		return fmt.Sprintf("Verified at %s: all %d checks passed", v.at.Format(time.TimeOnly), total)
	}
	return fmt.Sprintf("Verified at %s: %d of %d checks failed (details above)", v.at.Format(time.TimeOnly), total-passed, total)
}

// dialPort opens and closes one TCP connection; tests replace it.
var dialPort = func(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// parseUpstreamCheck reads `oberth upstream list --check`: an aligned table
// whose header names the columns ("KEY NAME" and "KEY FINGERPRINT" are two
// words each), then one row per upstream. AUTH is the last column and may
// hold an error with spaces, so it is read from the header's offset; it is
// empty when the server did not check.
func parseUpstreamCheck(out []byte) []upstreamCheck {
	authAt := -1
	header := false
	var rows []upstreamCheck
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !header {
			if fields[0] == "NAME" {
				header = true
				authAt = strings.Index(line, "AUTH")
			}
			continue
		}
		row := upstreamCheck{name: fields[0]}
		for _, f := range fields[1:] {
			if strings.Contains(f, "://") {
				row.url = f
				break
			}
		}
		if authAt >= 0 && len(line) > authAt {
			row.auth = strings.TrimSpace(line[authAt:])
		}
		rows = append(rows, row)
	}
	return rows
}

type verifyDoneMsg struct {
	result     verifyResult
	reRegister bool
}

// upstreamListHasRows reads `oberth upstream list`: a table whose header
// starts with NAME; any further non-empty line is a configured upstream.
func upstreamListHasRows(out []byte) bool {
	header := false
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
		case strings.HasPrefix(t, "NAME"):
			header = true
		case header:
			return true
		}
	}
	return false
}

func lastLine(out []byte, err error) string {
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		text = text[i+1:]
	}
	if text == "" && err != nil {
		text = err.Error()
	}
	return text
}

// verifyInstall checks the installed server: is an upstream registered
// (re-running the registration when asked — the only way to observe a key
// the forge accepted since), is the deployment Ready, and what upstream key
// it holds. It never reads a secret: the upstream key file read is the
// public half.
func verifyInstall(ctx context.Context, st WizardState, reRegister bool) verifyResult {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res := verifyResult{at: time.Now()}
	list := func() error {
		out, err := runKubectl(ctx, podExec(&st, "oberth", "upstream", "list", "--check")...)
		if err != nil && strings.Contains(string(out), "flag provided but not defined") {
			// A server older than --check: list without authenticating.
			out, err = runKubectl(ctx, podExec(&st, "oberth", "upstream", "list")...)
		}
		if err != nil {
			return fmt.Errorf("list upstreams: %s", lastLine(out, err))
		}
		res.upstream = upstreamListHasRows(out)
		res.upstreams = parseUpstreamCheck(out)
		return nil
	}
	if err := list(); err != nil {
		res.err = err.Error()
	}
	if !res.upstream && reRegister && st.ForgeOrg != "" && res.err == "" {
		out, err := runKubectl(ctx, upstreamAddArgs(&st)...)
		if err != nil {
			res.register = lastLine(out, err)
		}
		if err := list(); err != nil {
			res.err = err.Error()
		}
	}
	if st.ForgeAuth != forgeAuthExisting {
		if out, err := runKubectl(ctx, podExec(&st, "cat", upstreamPublicKeyPath)...); err == nil {
			if key, _, _, _, perr := ssh.ParseAuthorizedKey(out); perr == nil {
				res.publicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
				res.fingerprint = ssh.FingerprintSHA256(key)
			}
		}
	}
	checkClientSide(ctx, &st, &res)
	args := append(kubectlTarget(&st), "get", "deployment", "oberth", "-n", st.namespace(),
		"-o", `jsonpath={.status.readyReplicas}/{.spec.replicas} {.spec.template.spec.containers[0].image}`)
	out, err := runKubectl(ctx, args...)
	if err != nil {
		if res.err == "" {
			res.err = "read deployment: " + lastLine(out, err)
		}
		return res
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		counts := strings.SplitN(fields[0], "/", 2)
		if counts[0] == "" {
			counts[0] = "0" // readyReplicas is omitted while nothing is ready
		}
		res.ready = len(counts) == 2 && counts[0] != "0" && counts[0] == counts[1]
		res.readyText = strings.Join(counts, "/") + " ready"
	}
	if len(fields) > 1 {
		res.image = fields[1]
	}
	return res
}

// checkClientSide checks what a client on another machine meets: SSH and
// HTTPS answering on the advertised address, and, when the client files read
// the owner-only token file, a token in it (#058).
func checkClientSide(ctx context.Context, st *WizardState, res *verifyResult) {
	host := clientHost(st)
	var failed []string
	for _, port := range []string{"30022", "30443"} {
		if err := dialPort(ctx, net.JoinHostPort(host, port)); err != nil {
			failed = append(failed, port+": "+err.Error())
		}
	}
	res.portsChecked = true
	res.portsOK = len(failed) == 0
	if res.portsOK {
		res.portsText = "SSH 30022 and HTTPS 30443 answer on " + host
	} else {
		res.portsText = "not reachable on " + host + ": " + strings.Join(failed, "; ")
	}

	if st.ClientAccess == "none" {
		return
	}
	read, _ := installer.TokenCommands()
	if !strings.HasPrefix(read, "cat ") {
		return // a keychain, secret-tool or pass holds it; reading it may prompt
	}
	dir, err := installer.ClientConfigDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "token")
	res.tokenChecked = true
	data, err := os.ReadFile(path) //nolint:gosec // G304: the client config directory
	res.tokenOK = err == nil && strings.TrimSpace(string(data)) != ""
	if res.tokenOK {
		res.tokenText = "token stored in " + displayHome(path)
	} else {
		res.tokenText = "no token in " + displayHome(path) + " yet: the CLI and MCP cannot sign in"
	}
}

// clientFiles lists the client files the chosen access should have written.
func clientFiles(st *WizardState) []fileCheck {
	dir, err := installer.ClientConfigDir()
	if err != nil {
		return nil
	}
	var names []string
	switch st.ClientAccess {
	case "both":
		names = []string{"env", "mcp.json", "ca.crt"}
	case "cli":
		names = []string{"env", "ca.crt"}
	case "mcp":
		names = []string{"mcp.json", "ca.crt"}
	}
	var checks []fileCheck
	for _, n := range names {
		p := filepath.Join(dir, n)
		_, statErr := os.Stat(p)
		checks = append(checks, fileCheck{path: p, ok: statErr == nil})
	}
	return checks
}

// clientHost is the address a workstation client uses, mirroring the
// installer: localhost for kind and loopback API servers, else the API host.
func clientHost(st *WizardState) string {
	if st.Config.ClientHost != "" {
		return st.Config.ClientHost
	}
	if st.Config.CreateKind || st.ClusterInfo.server == "" {
		return "localhost"
	}
	u, err := url.Parse(st.ClusterInfo.server)
	if err != nil || u.Hostname() == "" || u.Hostname() == "localhost" {
		return "localhost"
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return "localhost"
	}
	return u.Hostname()
}

// ---------- the finish screen ----------

const (
	finVerify = iota
	finReport
	finLog
	finExit
)

// finishScreen is the end of the install: what state the server is in, what
// remains to do with the exact commands, client access, and Verify.
type finishScreen struct {
	focus     focusRing
	verifying bool
	verify    *verifyResult
	report    string
	reportErr string
}

func newFinishScreen(*wizard) *finishScreen {
	f := &finishScreen{focus: focusRing{buttons: 4}}
	f.focus.focusButton(finVerify)
	return f
}

// start runs the read-only half of Verify, so the screen shows the real
// state (and the upstream public key) without re-registering anything.
func (f *finishScreen) start(w *wizard) tea.Cmd {
	if !w.install.started && w.state.Install.Status == installNone {
		return nil
	}
	// --dry-mode promises no cluster calls; Verify execs kubectl.
	if w.opts.DryMode {
		return nil
	}
	return f.runVerify(w, false)
}

func (f *finishScreen) runVerify(w *wizard, reRegister bool) tea.Cmd {
	if w.state.Install.Status == installFailed {
		return nil
	}
	f.verifying = true
	st := w.state
	return func() tea.Msg {
		return verifyDoneMsg{result: verifyInstall(context.Background(), st, reRegister), reRegister: reRegister}
	}
}

func (f *finishScreen) update(w *wizard, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case verifyDoneMsg:
		f.verifying = false
		f.report = ""
		f.verify = &msg.result
		w.lastVerify = &msg.result
		if msg.result.ready && msg.result.upstream && w.state.Install.Status != installComplete {
			w.state.Install.Status = installComplete
			w.state.Install.Error = ""
			w.persist()
		}
		return nil
	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "esc":
			w.toMenu(stepInstall)
			return nil
		case "l":
			w.openLog()
			return nil
		case "enter":
			switch f.focus.button() {
			case finVerify:
				if !f.verifying && !w.opts.DryMode {
					return f.runVerify(w, true)
				}
			case finReport:
				path, err := writeReport(w)
				if err != nil {
					f.reportErr = "Report not saved: " + err.Error()
					f.report = ""
				} else {
					f.report, f.reportErr = path, ""
				}
			case finLog:
				w.openLog()
			case finExit:
				return w.quit(exitMenu)
			}
			return nil
		}
		if f.focus.handle(key) {
			return nil
		}
		switch key {
		case "up", "k":
			w.scroll--
		case "down", "j":
			w.scroll++
		case "pgup":
			w.scroll -= max(1, w.height-8)
		case "pgdown":
			w.scroll += max(1, w.height-8)
		}
	}
	return nil
}

// headline is the one-sentence state of the installation.
func headline(st *WizardState, v *verifyResult) string {
	switch st.Install.Status {
	case installComplete:
		if v != nil && v.ready {
			return "Oberth is installed and Ready."
		}
		return "Oberth is installed. Verify checks that the server is Ready."
	case installPendingForge:
		return "Oberth is installed, but " + st.ForgeType + " has not accepted its upstream key yet. Until it does, the server stays NotReady and clones fail."
	case installPartial:
		return "Oberth is installed, but onboarding did not finish. The log shows where it stopped; Verify re-checks."
	case installFailed:
		return "The last install failed at " + st.Install.Phase + ": " + st.Install.Error + ". Fix the step it names, then run Review → Continue again."
	}
	return "Oberth is not installed yet. Open Review to start the install."
}

// finishLines is the content of the finish screen and of the report, as
// plain text. Commands are kept whole; the screen wraps them.
func finishLines(w *wizard, v *verifyResult) []string {
	st := &w.state
	host := clientHost(st)
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }

	pendingForge := st.Install.Status == installPendingForge || (v != nil && !v.upstream && st.Install.Status == installPartial)
	if pendingForge {
		add("", "Finish the forge authorization (the upstream key: Oberth → "+st.ForgeType+"):")
		if v != nil && v.publicKey != "" {
			add("   upstream public key ("+v.fingerprint+"):", "   "+v.publicKey)
		}
		for _, l := range recoveryCommands(st) {
			add("   " + l)
		}
		add("   Verify (below) runs step 3 for you and checks readiness. Exiting keeps this pending state; rerun `oberth setup` to verify later.")
	}

	if v != nil {
		passed, total := v.checks()
		add("", fmt.Sprintf("Last verification (%s): %d of %d checks passed", v.at.Format(time.TimeOnly), passed, total))
		mark := func(ok bool) string {
			if ok {
				return "✓"
			}
			return "✗"
		}
		ready := "server Ready"
		if v.readyText != "" {
			ready += " (" + v.readyText + ")"
		}
		add("   " + mark(v.ready) + " " + ready)
		if v.image != "" {
			add("     image " + v.image)
		}
		if len(v.upstreams) == 0 {
			add("   " + mark(v.upstream) + " upstream registered")
		}
		for _, u := range v.upstreams {
			where := u.name + " (" + displayUpstream(u.url) + ")"
			switch {
			case u.ok():
				add("   ✓ upstream " + where + ": the forge accepts the key")
			case u.unchecked():
				add("   ✓ upstream " + where + " registered")
			default:
				add("   ✗ upstream "+where+": "+u.auth,
					"     add the upstream public key to that account, then Verify again")
			}
		}
		if v.register != "" {
			add("     registration: " + v.register)
		}
		if v.portsChecked {
			add("   " + mark(v.portsOK) + " " + v.portsText)
		}
		if v.tokenChecked {
			add("   " + mark(v.tokenOK) + " " + v.tokenText)
		}
		if v.err != "" {
			add("     " + v.err)
		}
	}

	if checks := clientFiles(st); len(checks) > 0 {
		add("", "Client access on this machine:")
		for _, c := range checks {
			if c.ok {
				add("   ✓ " + displayHome(c.path))
			} else {
				add("   ✗ " + displayHome(c.path) + " missing — the log says why")
			}
		}
		read, store := installer.TokenCommands()
		if v == nil || !v.tokenChecked || !v.tokenOK {
			add("   Store the bearer token from the credentials screen with:  "+store,
				"   The files read it back with:  "+read)
		}
		for _, c := range checks {
			if filepath.Base(c.path) == "env" {
				add("   Then:  . " + displayHome(c.path))
			}
		}
	} else if st.ClientAccess == "none" {
		add("", "Client access: not configured (chosen in step 6).")
	}

	if st.ForgeOrg != "" {
		if v == nil || !v.ready {
			add("", "Once the server is Ready:")
		}
		add("")
		add(installer.NextSteps(installer.NextStepsInput{
			Host:      host,
			Context:   st.contextForCommands(),
			Namespace: st.namespace(),
			Upstreams: nextStepsUpstreams(st, v),
			SSHAlias:  installer.SSHAliasConfigured(),
			// The pending forge authorization is explained above.
			OmitPendingHelp: pendingForge,
		})...)
	}
	if w.install.logPath != "" {
		add("", "Full install log (secrets masked): "+displayHome(w.install.logPath))
	}
	add("", "Verify the server out of band on every workstation:",
		"   TLS 30443:  "+serverCAFingerprintCommand(st),
		"   SSH 30022:  ssh-keyscan -p 30022 "+host,
		"", "Dashboard https://"+installer.URLHost(host)+":30443/runs · docs https://oberth.ci/docs")
	return lines
}

// nextStepsUpstreams are the organizations to offer: what the last Verify
// listed (with whether the forge accepts the key), else the answers.
func nextStepsUpstreams(st *WizardState, v *verifyResult) []installer.NextStepsUpstream {
	if v != nil && len(v.upstreams) > 0 {
		var ups []installer.NextStepsUpstream
		listed := map[string]bool{}
		for _, u := range v.upstreams {
			ups = append(ups, installer.NextStepsUpstream{Name: u.name, BaseURL: u.url, Connected: u.ok() || u.unchecked()})
			listed[orgOf(u.url)] = true
		}
		// A further organization whose key the forge has not accepted yet was
		// never registered, so the listing omits it; keep it as pending.
		for _, planned := range installer.PlanAlsoUpstreams(ups[0].Name, ups[0].BaseURL, st.Config.AlsoUpstreams) {
			if !listed[orgOf(planned.BaseURL)] {
				ups = append(ups, planned)
			}
		}
		return ups
	}
	ready := v != nil && v.upstream
	ups := []installer.NextStepsUpstream{{Name: st.ForgeType, BaseURL: "ssh://git@" + forgeUpstreamURL(st.ForgeType, st.ForgeOrg), Connected: ready}}
	return append(ups, installer.PlanAlsoUpstreams(st.ForgeType, ups[0].BaseURL, st.Config.AlsoUpstreams)...)
}

// serverCAFingerprintCommand prints the fingerprint clients should pin: the
// server CA when the server issues short-lived leaves, else the self-signed
// certificate.
func serverCAFingerprintCommand(st *WizardState) string {
	return kubectlLine(podExec(st, "sh", "-c", "cat /run/oberth-identities/server/ca.crt 2>/dev/null || cat /run/oberth-identities/server/tls.crt")) +
		" | openssl x509 -fingerprint -sha256 -noout"
}

func (f *finishScreen) frame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	spec := frameSpec{
		title:     stepInstall.title(),
		heading:   "Installation finished",
		explain:   headline(&w.state, f.verify),
		focusLine: -1,
		buttons:   []string{"Verify", "Save report", "Show log", "Exit"},
		button:    f.focus.button(),
		disabled:  map[int]bool{finVerify: f.verifying || w.state.Install.Status == installFailed || w.opts.DryMode},
		errMsg:    f.reportErr,
		keys:      "←/→ buttons · enter select · ↑/↓ pgup/pgdn scroll · l log · esc menu",
	}
	lines := finishLines(w, f.verify)
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for _, line := range lines {
		style := sText
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "✓"):
			style = sGo
		case strings.HasPrefix(trimmed, "✗"):
			style = sFail
		case strings.Contains(line, "kubectl ") || strings.Contains(line, "git clone") || strings.Contains(line, "ssh-keyscan") || strings.HasPrefix(trimmed, "ssh-"):
			style = sInfo
		case line != "" && !strings.HasPrefix(line, " "):
			style = sHighlight
		}
		if trimmed == "" {
			spec.body = append(spec.body, "")
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for i, l := range wrapWords(trimmed, inner-indent-2) {
			pad := strings.Repeat(" ", indent)
			if i > 0 {
				pad += "  "
			}
			spec.body = append(spec.body, pad+style.Render(l))
		}
	}
	switch {
	case f.verifying:
		spec.status = "Verifying…"
	case f.report != "":
		spec.status = "Report saved to " + displayHome(f.report)
	case f.verify != nil:
		spec.status = f.verify.summary()
	}
	return spec
}

// ---------- the saved report (#012) ----------

// writeReport saves a non-secret report next to setup.json under a
// timestamped name it creates exclusively, so no earlier report is ever
// overwritten. It returns the path.
func writeReport(w *wizard) (string, error) {
	statePath, err := setupStatePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	body := reportText(w)
	base := "setup-report-" + time.Now().Format("20060102-150405")
	for i := 1; i < 100; i++ {
		name := base + ".txt"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.txt", base, i)
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a name the wizard builds
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := file.WriteString(body); err != nil {
			_ = file.Close()
			return "", err
		}
		return path, file.Close()
	}
	return "", errors.New("too many reports with the same timestamp")
}

// reportText is the report body: answers, versions, per-phase status,
// failures, verification, recovery commands and the masked log tail. It
// reads only non-secret state; log lines went through the masker.
func reportText(w *wizard) string {
	st := &w.state
	var b strings.Builder
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(&b, format+"\n", a...) }
	p("Oberth setup report — %s", time.Now().Format(time.RFC3339))
	p("")
	p("State: %s", headline(st, w.lastVerify))
	p("oberth CLI: %s", valueOr(st.Config.BinaryVersion, "unknown"))
	switch {
	case st.Config.ChartPath != "":
		p("chart: local %s", st.Config.ChartPath)
	case st.Config.ChartVersion != "":
		p("chart: %s", st.Config.ChartVersion)
	default:
		p("chart: matching the CLI (or latest for a dev build)")
	}
	if w.lastVerify != nil && w.lastVerify.image != "" {
		p("server image: %s", w.lastVerify.image)
	}
	p("")
	p("Answers")
	for i := stepCluster; i < stepReview; i++ {
		p("  %d. %s: %s", int(i)+1, i.name(), valueOr(stepSummary(st, i), "not answered"))
	}
	p("  context: %s", valueOr(st.contextForCommands(), "(current)"))
	p("  endpoints: https://%s:30443 · ssh://git@%s:30022", installer.URLHost(clientHost(st)), installer.URLHost(clientHost(st)))
	p("")
	p("Install phases (attempt %d)", w.install.attempts)
	for _, step := range w.install.steps {
		p("  %-8s %s", step.status, step.name)
	}
	if st.Install.Error != "" {
		p("  result: %s — %s", st.Install.Status, st.Install.Error)
	}
	for _, f := range w.install.failures {
		p("  attempt %d failed at %s (%s): %s", f.attempt, f.phase, f.at.Format(time.TimeOnly), f.err)
	}
	p("")
	for _, line := range finishLines(w, w.lastVerify) {
		p("%s", line)
	}
	p("")
	p("Equivalent command:")
	p("%s", BuildCommandLine(st))
	if n := len(w.install.logLines); n > 0 {
		start := max(0, n-300)
		p("")
		p("Install log (secrets masked; last %d of %d lines)", n-start, n)
		for _, line := range w.install.logLines[start:] {
			p("  %s", line)
		}
	}
	return b.String()
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
