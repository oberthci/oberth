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
// zeroed as soon as the operator marks that entry saved.
type ceremonyEntry struct {
	label    string
	value    []byte
	revealed bool
	copied   bool
	saved    bool
}

const (
	cerReveal = iota
	cerCopy
	cerSaved
	cerContinue
)

// ceremony shows each credential once. Every entry is revealed or copied
// and acknowledged on its own; copying one entry no longer acknowledges —
// and discards — all of them (#010).
type ceremony struct {
	entries []*ceremonyEntry
	cursor  int
	focus   focusRing
	errMsg  string
}

func newCeremony() *ceremony {
	return &ceremony{focus: focusRing{targets: 1, buttons: 4}}
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
	c.cursor = len(c.entries) - 1
	c.focus = focusRing{targets: 1, buttons: 4}
}

// pending reports whether any credential still waits for acknowledgement.
func (c *ceremony) pending() bool {
	for _, e := range c.entries {
		if !e.saved {
			return true
		}
	}
	return false
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

func (c *ceremony) selected() *ceremonyEntry {
	if c.cursor < 0 || c.cursor >= len(c.entries) {
		return nil
	}
	return c.entries[c.cursor]
}

// act applies one of the ceremony's actions to the selected entry.
func (c *ceremony) act(action int) (tea.Cmd, bool) {
	e := c.selected()
	c.errMsg = ""
	switch action {
	case cerReveal:
		if e != nil && !e.saved {
			e.revealed = !e.revealed
		}
	case cerCopy:
		if e != nil && !e.saved && len(e.value) > 0 {
			// OSC 52 copy of the SELECTED credential only; it does not
			// reveal it (S8: opt-in, and the terminal may not support it).
			e.copied = true
			return tea.SetClipboard(string(e.value)), false
		}
	case cerSaved:
		if e == nil || e.saved {
			return nil, false
		}
		if !e.revealed && !e.copied {
			c.errMsg = "Reveal or copy " + e.label + " before marking it saved."
			return nil, false
		}
		e.saved = true
		e.revealed = false
		zero(e.value)
		e.value = nil
		for i, other := range c.entries {
			if !other.saved {
				c.cursor = i
				break
			}
		}
		if !c.pending() {
			c.focus.focusButton(cerContinue)
		}
	case cerContinue:
		if c.pending() {
			c.errMsg = "Mark every credential saved first — each is shown only on this screen."
			return nil, false
		}
		return nil, true
	}
	return nil, false
}

// update handles a key; leave reports that every entry is acknowledged and
// the operator continued. There is no Go Back: the values cannot come back.
func (c *ceremony) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	key := msg.String()
	switch key {
	case "r":
		return c.act(cerReveal)
	case "c":
		return c.act(cerCopy)
	case "s":
		return c.act(cerSaved)
	case "enter":
		if b := c.focus.button(); b >= 0 {
			return c.act(b)
		}
		return c.act(cerSaved)
	}
	if c.focus.handle(key) || c.focus.button() >= 0 {
		return nil, false
	}
	switch key {
	case "up", "k":
		c.cursor = max(0, c.cursor-1)
	case "down", "j":
		c.cursor = min(len(c.entries)-1, c.cursor+1)
	}
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
		return "Uplink bearer token: the CLI and MCP clients authenticate with it. Oberth keeps only a digest, so this is the only copy. Store it where the client files read it: " + store
	}
	return "Store this value now; it is not shown again."
}

func (c *ceremony) frame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	spec := frameSpec{
		title:     stepInstall.title(),
		heading:   "Store the credentials",
		explain:   "Each value exists only on this screen. Reveal or copy one, store it, then mark it saved — that discards it here. Continue unlocks once every value is saved. Copy uses the terminal clipboard (OSC 52), which is outside Oberth's control.",
		focusLine: -1,
		errMsg:    c.errMsg,
		buttons:   []string{"Reveal", "Copy", "Saved", "Continue"},
		button:    c.focus.button(),
		disabled:  map[int]bool{cerContinue: c.pending()},
		keys:      "↑/↓ select · r reveal · c copy · s saved · tab buttons",
	}
	for i, e := range c.entries {
		marker := "  "
		label := sText.Render(e.label)
		if i == c.cursor {
			marker = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
			label = sSelected.Render(e.label)
			// Keep the value line in view, not just the label.
			spec.focusLine = len(spec.body) + 1
		}
		var state string
		switch {
		case e.saved:
			state = sGo.Render("✓ saved — discarded here")
		case e.copied:
			state = sHold.Render("copied · not marked saved")
		default:
			state = sHold.Render("not saved yet")
		}
		spec.body = append(spec.body, marker+label+"  "+state)
		switch {
		case e.saved:
		case e.revealed:
			for _, l := range wrapText(string(e.value), inner-4) {
				spec.body = append(spec.body, "    "+sReveal.Render(l))
			}
		default:
			spec.body = append(spec.body, "    "+sHighlight.Render(strings.Repeat("█", min(40, inner-4))))
		}
		for _, l := range wrapText(credentialPurpose(e.label), inner-4) {
			spec.body = append(spec.body, "    "+sMuted.Render(l))
		}
		spec.body = append(spec.body, "")
	}
	return spec
}
