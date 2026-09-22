package setuptui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/crypto/ssh"
)

type sshKey struct {
	path        string
	algorithm   string
	fingerprint string
}

type uplinkPage struct {
	identity   string
	sshKeys    []sshKey
	keyCursor  int
	focusField int // 0=identity, 1=key list
	errMsg     string
}

func newUplinkPage() *uplinkPage {
	return &uplinkPage{}
}

func (p *uplinkPage) title() string    { return "crew manifest" }
func (p *uplinkPage) question() string { return "Who are you?" }
func (p *uplinkPage) keys() string {
	keys := sKey.Render("↑/↓") + " move · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
	if len(p.sshKeys) == 0 {
		keys += " · " + sKey.Render("r") + " rescan keys"
	}
	return keys
}

func (p *uplinkPage) init(state *WizardState) tea.Cmd {
	if state.UplinkIdentity != "" {
		p.identity = state.UplinkIdentity
	} else {
		// Default identity from hostname.
		user := os.Getenv("USER")
		if user == "" {
			user = "admin"
		}
		host, _ := os.Hostname()
		if host == "" {
			host = "localhost"
		}
		p.identity = user + "@" + host
	}

	// Scan ~/.ssh for public keys (S9: private keys are never read).
	p.sshKeys = scanSSHPublicKeys()
	p.keyCursor = 0
	p.focusField = 0
	p.errMsg = ""

	// Pre-select if state already has a key path.
	if state.SSHKeyPath != "" {
		for i, k := range p.sshKeys {
			if k.path == state.SSHKeyPath {
				p.keyCursor = i
				break
			}
		}
	}

	return nil
}

func (p *uplinkPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "tab":
			p.focusField = (p.focusField + 1) % 2
		case "shift+tab":
			p.focusField = (p.focusField + 1) % 2
		case "up", "k":
			// Guard: 'k' is text input when the identity field is focused.
			if p.focusField == 0 {
				if msg.String() == "k" {
					p.identity += "k"
				}
				return p, nil // identity is the top of the page — nowhere further up
			}
			// In the key list: walk up, and leave the list from its first
			// entry back into the identity field.
			if p.keyCursor > 0 {
				p.keyCursor--
			} else {
				p.focusField = 0
			}
		case "down", "j":
			// Guard: 'j' is text input when the identity field is focused.
			if p.focusField == 0 {
				if msg.String() == "j" {
					p.identity += "j"
					return p, nil
				}
				p.focusField = 1 // down from identity enters the key list
				return p, nil
			}
			if p.keyCursor < len(p.sshKeys)-1 {
				p.keyCursor++
			}
		case "enter":
			if p.identity == "" {
				p.errMsg = "identity is required"
				return p, nil
			}
			if err := validateUplinkIdentityTUI(p.identity); err != nil {
				p.errMsg = err.Error()
				return p, nil
			}
			if len(p.sshKeys) == 0 {
				p.errMsg = "no SSH public keys found — generate one: ssh-keygen -t ed25519 — then press r to rescan"
				return p, nil
			}
			state.UplinkIdentity = p.identity
			state.SSHKeyPath = p.sshKeys[p.keyCursor].path
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "r":
			// Rescan ~/.ssh for public keys — useful after generating a key
			// in another terminal without leaving the wizard.
			if p.focusField == 0 {
				// 'r' on the identity field is text input, not a hotkey.
				p.identity += "r"
				return p, nil
			}
			p.sshKeys = scanSSHPublicKeys()
			p.keyCursor = 0
			p.errMsg = ""
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		case "backspace":
			if p.focusField == 0 && len(p.identity) > 0 {
				p.identity = p.identity[:len(p.identity)-1]
			}
		default:
			text := msg.String()
			if p.focusField == 0 && len(text) == 1 {
				p.identity += text
			}
		}
	}
	return p, nil
}

func (p *uplinkPage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	// Identity field.
	idCursor := "  "
	idLabelStyle := sMuted
	if p.focusField == 0 {
		idCursor = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
		idLabelStyle = lipgloss.NewStyle().Foreground(cPurple)
	}
	idInput := inputBox(p.identity, "name@machine", p.focusField == 0)
	_, _ = fmt.Fprintf(&b, "  %s%s %s\n", idCursor, idLabelStyle.Render(fmt.Sprintf("%-12s", "identity")), idInput)
	b.WriteString("    " + sMuted.Render("name@machine — every push is linked to this identity") + "\n\n")

	// SSH public key list.
	b.WriteString("  " + sMuted.Render("ssh public key") + "   " +
		sMuted.Render("(~/.ssh — ") + sGo.Render("private keys are never read") + sMuted.Render(")") + "\n")

	if len(p.sshKeys) == 0 {
		b.WriteString("  " + sFail.Render("  no public keys found in ~/.ssh") + "\n")
		b.WriteString("  " + sMuted.Render("  generate one:  ssh-keygen -t ed25519") + "\n")
		b.WriteString("  " + sMuted.Render("  then press r to rescan") + "\n")
	} else {
		// Size the name column to the longest key file so every row's
		// algorithm and fingerprint start in the same column.
		nameWidth := 0
		for _, k := range p.sshKeys {
			nameWidth = max(nameWidth, lipgloss.Width(filepath.Base(k.path)))
		}
		nameWidth = min(nameWidth, 32)

		var listContent strings.Builder
		for i, k := range p.sshKeys {
			cursor := "  "
			if i == p.keyCursor {
				cursor = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
			}
			name := padTo(truncateRunes(filepath.Base(k.path), nameWidth), nameWidth)
			algo := sInfo.Render(fmt.Sprintf("%-8s", k.algorithm))
			fp := sText.Render(k.fingerprint)
			_, _ = fmt.Fprintf(&listContent, "  %s%s %s  %s\n", cursor, name, algo, fp)
		}

		// Indent every line of the box, not just the first — a bordered
		// block is several lines, and the border must stay one column.
		box := sListBox.Render(listContent.String())
		for _, line := range strings.Split(box, "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	// Command preview.
	if len(p.sshKeys) > 0 && p.keyCursor < len(p.sshKeys) {
		keyPath := p.sshKeys[p.keyCursor].path
		cmd := fmt.Sprintf("oberth uplink add - %s < %s", p.identity, keyPath)
		b.WriteString("\n  " + sMuted.Render("will run: ") + sInfo.Render(cmd) + "\n")
	}

	if p.errMsg != "" {
		b.WriteString("\n  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}

// scanSSHPublicKeys looks for *.pub files in ~/.ssh.
// Only public key files are read (S9).
func scanSSHPublicKeys() []sshKey {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	sshDir := filepath.Join(home, ".ssh")

	entries, err := os.ReadDir(sshDir)
	if err != nil {
		return nil
	}

	var keys []sshKey
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		path := filepath.Join(sshDir, entry.Name())
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from ~/.ssh directory listing, not user input
		if err != nil {
			continue
		}

		// Parse as a real authorized_keys entry. Anything that does not
		// parse is skipped outright — a mislabeled or corrupt file must not
		// be offered as an uplink key, and its content must not be echoed.
		pk, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}

		// The REAL SHA256 fingerprint — the same value `ssh-keygen -lf`
		// prints, so out-of-band verification against the server's records
		// compares like with like. Never display raw key-material prefixes
		// dressed up as a fingerprint.
		fp := ssh.FingerprintSHA256(pk)
		if len(fp) > 20 {
			fp = fp[:14] + "…" + fp[len(fp)-4:]
		}

		keys = append(keys, sshKey{
			path:        path,
			algorithm:   strings.TrimPrefix(pk.Type(), "ssh-"),
			fingerprint: fp,
		})
	}

	return keys
}
