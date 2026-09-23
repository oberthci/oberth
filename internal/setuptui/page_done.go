package setuptui

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type donePage struct {
	totalSteps       int
	greenCount       int
	skippedCount     int
	fingerprint      string
	sshFingerprint   string
	context          string
	identity         string
	reportSaved      bool
	deployKeyPending bool
}

func (p *donePage) title() string    { return "orbit" }
func (p *donePage) question() string { return "" }
func (p *donePage) keys() string {
	if p.reportSaved {
		return sKey.Render("enter") + " exit · " + sMuted.Render("report saved")
	}
	return sKey.Render("enter") + " exit · " + sKey.Render("s") + " save report (contains no secrets)"
}

func (p *donePage) init(state *WizardState) tea.Cmd {
	p.context = state.SelectedContext
	p.identity = state.UplinkIdentity

	// totalSteps and greenCount are populated by wizard.advance() from
	// the apply page's real step data before this init is called.
	return nil
}

func (p *donePage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter", "q":
			return p, tea.Quit
		case "s":
			if !p.reportSaved {
				p.reportSaved = p.saveReport(state)
			}
			return p, nil
		}
	}
	return p, nil
}

// saveReport writes a plaintext setup summary to ./oberth-setup-report.txt.
// Contains no secrets by construction: only config fields are read. Returns
// true on success.
func (p *donePage) saveReport(state *WizardState) bool {
	var b strings.Builder
	b.WriteString("Oberth Setup Report\n")
	b.WriteString(strings.Repeat("=", 40) + "\n\n")
	fmt.Fprintf(&b, "Steps: %d/%d green\n", p.greenCount, p.totalSteps)
	fmt.Fprintf(&b, "Context: %s\n", p.context)
	fmt.Fprintf(&b, "Identity: %s\n", p.identity)
	ns := state.Config.Namespace
	if ns == "" {
		ns = "oberth"
	}
	fmt.Fprintf(&b, "Namespace: %s\n", ns)
	if state.Config.ArgoNamespace != "" {
		fmt.Fprintf(&b, "Argo namespace: %s\n", state.Config.ArgoNamespace)
	}
	openbaoNs := state.Config.OpenBaoNamespace
	if openbaoNs == "" {
		openbaoNs = "openbao"
	}
	fmt.Fprintf(&b, "OpenBao namespace: %s\n", openbaoNs)
	if state.ForgeType != "" {
		fmt.Fprintf(&b, "Forge: %s / %s\n", state.ForgeType, state.ForgeOrg)
	}
	if state.StoreAddress != "" {
		fmt.Fprintf(&b, "Store: %s\n", state.StoreAddress)
	}
	fmt.Fprintf(&b, "TLS: %s\n", state.TLSMode)
	b.WriteString("\nFingerprints\n")
	b.WriteString(strings.Repeat("-", 40) + "\n")
	if p.fingerprint != "" {
		fmt.Fprintf(&b, "TLS 30443: %s\n", p.fingerprint)
	} else {
		fmt.Fprintf(&b, "TLS 30443: kubectl get secret -n %s oberth-tls -o jsonpath='{.data.tls\\.crt}' | base64 -d | openssl x509 -fingerprint -sha256 -noout\n", ns)
	}
	if p.sshFingerprint != "" {
		fmt.Fprintf(&b, "SSH 30022: %s\n", p.sshFingerprint)
	} else {
		b.WriteString("SSH 30022: ssh-keyscan -p 30022 <node-address>\n")
	}
	// #nosec G306 -- report is non-sensitive, world-readable is fine.
	return os.WriteFile("oberth-setup-report.txt", []byte(b.String()), 0644) == nil
}

func (p *donePage) view(state *WizardState, width, _ int) string {
	_ = width
	var b strings.Builder

	// Completion line — reflects actual step results, never fabricated.
	// Steps the installer skipped (not reported by StepProgressSink) are
	// not failures — they just were not applicable to this configuration.
	completedCount := p.greenCount + p.skippedCount
	greenStr := fmt.Sprintf("%d/%d", p.greenCount, p.totalSteps)
	if completedCount >= p.totalSteps && p.totalSteps > 0 {
		b.WriteString("  " + sText.Render("Setup complete — ") +
			sGo.Render(greenStr+" steps green") + sText.Render(".") + "\n\n")
	} else if p.totalSteps > 0 {
		b.WriteString("  " + sText.Render("Setup finished with errors — ") +
			sFail.Render(greenStr+" steps green") + sText.Render(".") + "\n\n")
	} else {
		b.WriteString("  " + sText.Render("Setup complete.") + "\n\n")
	}

	// Running now.
	ns := state.Config.Namespace
	if ns == "" {
		ns = "oberth"
	}
	openbaoNs := state.Config.OpenBaoNamespace
	if openbaoNs == "" {
		openbaoNs = "openbao"
	}
	runningLine := sText.Render("deployment oberth (ns " + ns + ")")
	if state.StoreMode != "connect" {
		runningLine += sMuted.Render(" · openbao (ns " + openbaoNs + ")")
	}
	b.WriteString("  " + sMuted.Render("running now") + "    " + runningLine + "\n\n")

	// Fingerprints section.
	b.WriteString("  " + sMuted.Render("verify out of band on every workstation") + "\n")

	if p.fingerprint != "" {
		b.WriteString("    " + sMuted.Render("tls 30443") + "    " +
			sHighlight.Render(p.fingerprint) + "\n")
	} else {
		b.WriteString("    " + sMuted.Render("tls 30443") + "    " +
			sMuted.Render("(retrieve with the command below)") + "\n")
	}

	b.WriteString("      " + sInfo.Render("kubectl get secret -n "+ns+" oberth-tls -o jsonpath='{.data.tls\\.crt}' | base64 -d | openssl x509 -fingerprint -sha256 -noout") + "\n")

	if p.sshFingerprint != "" {
		b.WriteString("    " + sMuted.Render("ssh 30022") + "    " +
			sHighlight.Render(p.sshFingerprint) + "\n")
	} else {
		b.WriteString("    " + sMuted.Render("ssh 30022") + "    " +
			sMuted.Render("(retrieve with the command below)") + "\n")
	}

	// Use node IP from cluster info for the ssh-keyscan command.
	scanTarget := "localhost"
	if state.ClusterInfo.nodeIP != "" {
		scanTarget = state.ClusterInfo.nodeIP
	}
	b.WriteString("      " + sInfo.Render("ssh-keyscan -p 30022 "+scanTarget) + "\n\n")

	// Deploy key pending — the server stays NotReady until the key is
	// registered at the forge. Show retrieval guidance before next steps.
	if p.deployKeyPending {
		b.WriteString("  " + sFail.Render("deploy key pending") + "    " +
			sText.Render("register it at the forge, then the server will become ready") + "\n")
		b.WriteString("    " + sInfo.Render("kubectl get secret -n "+ns+" oberth-upstream-key -o jsonpath='{.data.id_ed25519\\.pub}' | base64 -d") + "\n\n")
	}

	// Next steps.
	b.WriteString("  " + sMuted.Render("next") + "\n")
	gitTarget := "localhost"
	if state.ClusterInfo.nodeIP != "" {
		gitTarget = state.ClusterInfo.nodeIP
	}
	b.WriteString("    " + sInfo.Render("git clone ssh://git@"+gitTarget+":30022/oberth.git") + "\n")

	mcpAddr := "https://localhost:30443/mcp"
	if state.ClusterInfo.nodeIP != "" {
		mcpAddr = "https://" + state.ClusterInfo.nodeIP + ":30443/mcp"
	}
	b.WriteString("    " + sMuted.Render(".claude/settings.local.json → ") +
		sInfo.Render(mcpAddr) +
		sMuted.Render("   (token from the ceremony)") + "\n")
	b.WriteString("    " + sMuted.Render("push — ") +
		sGo.Render("green") + sMuted.Render(" publishes upstream · ") +
		sFail.Render("red") + sMuted.Render(" opens exactly one ci issue") + "\n\n")

	// Dashboard and docs.
	dashAddr := "https://localhost:30443/runs"
	if state.ClusterInfo.nodeIP != "" {
		dashAddr = "https://" + state.ClusterInfo.nodeIP + ":30443/runs"
	}
	b.WriteString("  " + sMuted.Render("dashboard ") +
		sInfo.Render(dashAddr) +
		sMuted.Render(" · docs ") +
		sInfo.Render("https://oberth.ci/docs") + "\n")

	return b.String()
}
