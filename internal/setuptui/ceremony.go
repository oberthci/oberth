package setuptui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/oberthci/oberth/internal/installer"
)

// ceremonyEntry is one once-only credential: the OpenBao root token, an
// unseal key, or the uplink bearer token. The value lives in one []byte
// owned by the ceremony — never in WizardState, never logged — and is
// zeroed as soon as the operator continues past it.
type ceremonyEntry struct {
	label string
	value []byte
	saved bool
	// filedAt is where Save to file put the value (#062).
	filedAt string
}

const (
	cerFile = iota
	cerContinue
	cerButtons
)

// saveTokenFile writes the bearer token where the client files read it;
// tests replace it.
var saveTokenFile = installer.SaveTokenFile

// tokenFileFallback reports whether this host's client files read the token
// from the owner-only file; tests replace it.
var tokenFileFallback = installer.TokenFileFallback

// fileable reports whether Save to file applies to an entry: only the bearer
// token, only where the client files read it from the owner-only file.
func fileable(e *ceremonyEntry) bool {
	return e != nil && !e.saved && e.filedAt == "" && strings.HasPrefix(e.label, "Bearer token") && tokenFileFallback()
}

// ceremony shows every credential in full on one screen. There is nothing
// to reveal, copy or tick off per value (#071): the operator selects the
// text in the terminal, and Continue asks once more before it discards them.
type ceremony struct {
	entries []*ceremonyEntry
	focus   focusRing
	errMsg  string
	// confirming is set by the first Continue; the second one leaves.
	confirming bool
}

func newCeremony() *ceremony {
	c := &ceremony{focus: focusRing{buttons: cerButtons}}
	c.focus.focusButton(cerContinue)
	return c
}

// add takes ownership of one credential: it copies the bytes and ZEROS THE
// SOURCE (S2: the delivery buffer must not linger on the heap).
func (c *ceremony) add(label string, value []byte) {
	cp := make([]byte, len(value))
	copy(cp, value)
	for i := range value {
		value[i] = 0
	}
	c.entries = append(c.entries, &ceremonyEntry{label: label, value: cp})
	c.confirming, c.errMsg = false, ""
	c.focus.focusButton(cerContinue)
}

// pending reports whether any credential has not been shown and continued
// past yet.
func (c *ceremony) pending() bool {
	return len(c.unsaved()) > 0
}

func (c *ceremony) unsaved() []*ceremonyEntry {
	var out []*ceremonyEntry
	for _, e := range c.entries {
		if !e.saved {
			out = append(out, e)
		}
	}
	return out
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// zeroAll zeros every credential buffer (S2: teardown discipline).
func (c *ceremony) zeroAll() {
	for _, e := range c.entries {
		zero(e.value)
		e.value = nil
	}
}

// bearer is the unsaved bearer token Save to file applies to, if any.
func (c *ceremony) bearer() *ceremonyEntry {
	for _, e := range c.unsaved() {
		if fileable(e) {
			return e
		}
	}
	return nil
}

// act applies one of the ceremony's actions.
func (c *ceremony) act(action int) (tea.Cmd, bool) {
	switch action {
	case cerFile:
		// Oberth writes a token to disk only here, when the operator asks
		// (docs/remote-cli.md, #062).
		e := c.bearer()
		if e == nil {
			c.errMsg = "Save to file is for the bearer token on a host whose client files read ~/.config/oberth/token."
			return nil, false
		}
		path, err := saveTokenFile(e.value)
		if err != nil {
			c.errMsg = "Not saved: " + err.Error()
			return nil, false
		}
		e.filedAt, c.errMsg = path, ""
		c.focus.focusButton(cerContinue)
	case cerContinue:
		if !c.pending() {
			return nil, true
		}
		if !c.confirming {
			c.confirming = true
			c.errMsg = "These values are not shown again once you continue. Store every one of them, then press Enter again."
			return nil, false
		}
		for _, e := range c.unsaved() {
			e.saved = true
			zero(e.value)
			e.value = nil
		}
		c.confirming, c.errMsg = false, ""
		return nil, true
	}
	return nil, false
}

// update handles a key; leave reports that the operator continued past
// every value. There is no Go Back: the values cannot come back. The arrow
// keys scroll the screen (installScreen.update).
func (c *ceremony) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	key := msg.String()
	switch key {
	case "f":
		return c.act(cerFile)
	case "enter":
		if b := c.focus.button(); b >= 0 {
			return c.act(b)
		}
		return c.act(cerContinue)
	}
	c.focus.handle(key)
	return nil, false
}

// credentialPurpose says what one credential is for and where it belongs.
// Only the bearer token is kept server-side as a digest (#010).
func credentialPurpose(label string) string {
	switch {
	case strings.HasPrefix(label, "Root token"):
		return "OpenBao root token: full administrative access to the secret store. Not needed day to day — keep it offline, e.g. in a password manager or a sealed envelope."
	case strings.HasPrefix(label, "Unseal key"):
		return "OpenBao unseal key: needed whenever OpenBao restarts sealed (`kubectl exec -it -n <openbao-namespace> openbao-0 -- bao operator unseal`, or the opt-in deploy/systemd/oberth-openbao-unseal watcher). Keep it apart from the root token."
	case strings.HasPrefix(label, "Bearer token"):
		_, store := installer.TokenCommands()
		text := "Uplink bearer token: the CLI and MCP clients authenticate with it. Oberth keeps only a digest, so this is the only copy. Store it where the client files read it: " + store
		if tokenFileFallback() {
			text += " — or press Save to file (f), which writes exactly that file (mode 0600)."
		}
		return text
	}
	return "Store this value now; it is not shown again."
}

func (c *ceremony) frame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	spec := frameSpec{
		title:     stepInstall.title(),
		heading:   "Store the credentials",
		explain:   "These values exist only on this screen. Select each one with the mouse and store it now, e.g. in a password manager. Continue discards them here, after one more warning.",
		focusLine: -1,
		errMsg:    c.errMsg,
		buttons:   []string{"Save to file", "Continue"},
		button:    c.focus.button(),
		disabled:  map[int]bool{cerFile: c.bearer() == nil},
		keys:      "↑/↓ pgup/pgdn scroll · f save token to file · ←/→ buttons · enter continue",
	}
	for _, e := range c.entries {
		label := sSelected.Render(e.label)
		var state string
		switch {
		case e.saved:
			state = sGo.Render("✓ shown earlier — discarded here")
		case e.filedAt != "":
			state = sGo.Render("✓ saved to " + displayHome(e.filedAt) + " (0600)")
		}
		spec.body = append(spec.body, lipgloss.NewStyle().Foreground(cPurple).Render("• ")+label+"  "+state)
		if !e.saved {
			for _, l := range wrapText(string(e.value), inner-4) {
				spec.body = append(spec.body, "    "+sReveal.Render(l))
			}
		}
		for _, l := range wrapText(credentialPurpose(e.label), inner-4) {
			spec.body = append(spec.body, "    "+sMuted.Render(l))
		}
		spec.body = append(spec.body, "")
	}
	return spec
}
