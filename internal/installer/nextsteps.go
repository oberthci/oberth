package installer

import (
	"fmt"
	"io"
	"strings"
)

// NextStepsUpstream is one organization the server is connected to.
type NextStepsUpstream struct {
	Name      string
	BaseURL   string
	Connected bool
}

// NextStepsInput describes an installed server for NextSteps.
type NextStepsInput struct {
	// Host is the address clients use (Config.ClientHost or the default);
	// empty means localhost.
	Host      string
	Context   string
	Namespace string
	Upstreams []NextStepsUpstream
	// SSHAlias is set when ~/.ssh/config on this machine has the "oberth"
	// Host block, so the short clone form works here.
	SSHAlias bool
	// OmitPendingHelp drops the "Finish <upstream>" commands, for a caller
	// that already explains the pending forge authorization above.
	OmitPendingHelp bool
}

// NextSteps is what to do after an install, as plain text lines: clone from
// each connected organization, start CI, connect another organization, and
// give another machine or agent an uplink, each with the exact command for
// this server (#060). The setup finish screen and the installer print the
// same lines. Lines without indentation are headings.
func NextSteps(in NextStepsInput) []string {
	host := in.Host
	if host == "" {
		host = "localhost"
	}
	local := host == "localhost"
	host = URLHost(host)
	ns := in.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	exec := func(stdin bool, command ...string) string {
		parts := []string{"kubectl"}
		if in.Context != "" {
			parts = append(parts, "--context", in.Context)
		}
		parts = append(parts, "exec")
		if stdin {
			parts = append(parts, "-i")
		}
		parts = append(parts, "-n", ns, "deploy/oberth", "-c", "oberth", "--", "/usr/local/bin/oberth")
		return strings.Join(append(parts, command...), " ")
	}

	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }

	var pending []NextStepsUpstream
	firstRepo := ""
	add("Clone a repository through Oberth (the first clone registers it):")
	for _, u := range in.Upstreams {
		repo := u.Name + "/" + orgFromBaseURL(u.BaseURL) + "/<repo>"
		add("   git clone ssh://git@" + host + ":" + sshNodePort + "/" + repo + ".git")
		if !u.Connected {
			pending = append(pending, u)
			add("     (once " + hostnameFromURL(u.BaseURL) + " accepts the upstream key)")
			continue
		}
		if firstRepo == "" {
			firstRepo = repo
		}
	}
	if firstRepo != "" && in.SSHAlias {
		add("   on this machine also:  git clone git@oberth:" + firstRepo + ".git")
	}

	add("", "Run CI: push a branch. A repository without a pipeline gets one from oberth init:",
		"   cd <repo> && oberth init && git push origin HEAD:my-branch",
		"   oberth runs --ref my-branch   ·   https://"+host+":"+httpsNodePort+"/runs")

	if in.OmitPendingHelp {
		pending = nil
	}
	for _, u := range pending {
		add("", "Finish "+u.Name+" ("+hostnameFromURL(u.BaseURL)+"/"+orgFromBaseURL(u.BaseURL)+"): add the upstream public key to that account, then:",
			"   "+exec(false, "upstream", "add", "--yes", "--no-wait", u.Name, u.BaseURL))
	}

	other, otherURL := "codeberg", "ssh://git@codeberg.org/<org>"
	for _, u := range in.Upstreams {
		if hostnameFromURL(u.BaseURL) == "codeberg.org" {
			other, otherURL = "github", "ssh://git@github.com/<org>"
			break
		}
	}
	add("", "Connect another organization with the same upstream key (add the key to that forge account first):",
		"   "+exec(false, "upstream", "add", "--yes", other, otherURL),
		"   upstream list --check shows which forges accept the key")

	add("", "Give another machine or agent access (an uplink):",
		"   1. there:  cat ~/.ssh/id_ed25519.pub   and copy it here as you@laptop.pub",
		"   2. here:   "+exec(true, "uplink", "add", "-", "you@laptop")+" < you@laptop.pub",
		"   3. there:  export OBERTH_BASE_URL=https://"+host+":"+httpsNodePort+" OBERTH_CA_CERT=<a copy of ~/.config/oberth/ca.crt>",
		"              and store the token step 2 printed once (docs/remote-cli.md, docs/mcp-setup.md)")
	if local {
		add("", "localhost works on this machine only. For other machines, rerun oberth setup and pick this host's address on the TLS step (oberth install --client-host <address>).")
	}
	return lines
}

// readyURL is the dashboard address clients are told to open.
func readyURL(deps Deps) string {
	return "https://" + URLHost(clientReachableHost(deps)) + ":" + httpsNodePort + "/runs"
}

// printReadyWithNextSteps prints the Ready banner followed by NextSteps.
func printReadyWithNextSteps(w io.Writer, cfg Config, deps Deps, upstreams []NextStepsUpstream, sshConfigured, color bool) {
	if color {
		_, _ = fmt.Fprintf(w, "\n\033[1;32mReady\033[0m — %s\n", readyURL(deps))
	} else {
		_, _ = fmt.Fprintf(w, "\nReady — %s\n", readyURL(deps))
	}
	_, _ = fmt.Fprintln(w)
	for _, line := range NextSteps(NextStepsInput{
		Host:      clientReachableHost(deps),
		Context:   deps.ContextName,
		Namespace: cfg.Namespace,
		Upstreams: upstreams,
		SSHAlias:  sshConfigured,
	}) {
		_, _ = fmt.Fprintln(w, "  "+line)
	}
	_, _ = fmt.Fprintln(w)
}

// PlanAlsoUpstreams names the --also-upstream organizations the way
// onboarding will register them, for a summary written before (or without)
// asking the server. Duplicates of an earlier organization are dropped.
func PlanAlsoUpstreams(firstName, firstURL string, also []string) []NextStepsUpstream {
	used, orgs := map[string]bool{}, map[string]bool{}
	if firstName != "" {
		used[firstName] = true
		orgs[orgFromBaseURL(firstURL)] = true
	}
	var planned []NextStepsUpstream
	for _, baseURL := range also {
		org := orgFromBaseURL(baseURL)
		if orgs[org] {
			continue
		}
		orgs[org] = true
		name := alsoUpstreamName(baseURL, used, orgs)
		used[name] = true
		planned = append(planned, NextStepsUpstream{Name: name, BaseURL: baseURL})
	}
	return planned
}

// SSHAliasConfigured reports whether ~/.ssh/config has the "oberth" Host
// block onboarding writes, so the short clone form works on this machine.
func SSHAliasConfigured() bool { return sshHostBlockExists() }
