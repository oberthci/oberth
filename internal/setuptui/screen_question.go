package setuptui

import (
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// probeDoneMsg carries a question's slow check back to the UI goroutine.
type probeDoneMsg struct {
	id     string
	result probeResult
}

const (
	buttonBack     = 0
	buttonContinue = 1
)

// questionScreen renders one question: a choice list or text fields.
type questionScreen struct {
	q      *question
	opts   []option
	cursor int
	inputs []*lineInput
	focus  focusRing
	errMsg string
	notice string
	busy   bool
}

func newQuestionScreen(q *question, st *WizardState) *questionScreen {
	s := &questionScreen{q: q}
	if q.isChoice() {
		s.opts = q.options(st)
		sel := q.selected(st)
		for i, o := range s.opts {
			if o.value == sel {
				s.cursor = i
			}
		}
		s.focus = focusRing{targets: 1, buttons: 2}
		if len(s.opts) == 0 {
			s.focus = focusRing{targets: 0, buttons: 2, at: buttonBack}
		}
	} else {
		for _, v := range q.values(st) {
			s.inputs = append(s.inputs, newLineInput(v))
		}
		s.focus = focusRing{targets: len(s.inputs), buttons: 2}
	}
	return s
}

// answer is what the screen currently holds.
func (s *questionScreen) answer() []string {
	if s.q.isChoice() {
		if len(s.opts) == 0 {
			return []string{""}
		}
		return []string{s.opts[s.cursor].value}
	}
	values := make([]string, len(s.inputs))
	for i, in := range s.inputs {
		values[i] = in.value()
	}
	return values
}

func (s *questionScreen) submit(w *wizard) tea.Cmd {
	if err := s.q.save(&w.state, s.answer()); err != nil {
		if errors.Is(err, errRescan) {
			s.opts = s.q.options(&w.state)
			s.cursor = 0
			s.errMsg = ""
			s.notice = "Scanned ~/.ssh again at " + time.Now().Format(time.TimeOnly)
			return nil
		}
		s.errMsg = err.Error()
		return nil
	}
	s.errMsg = ""
	if s.q.probe != nil {
		if work := s.q.probe(&w.state); work != nil {
			s.busy = true
			id := s.q.id
			return func() tea.Msg { return probeDoneMsg{id: id, result: work()} }
		}
	}
	return w.nextQuestion(s.q)
}

func (s *questionScreen) update(w *wizard, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case probeDoneMsg:
		if msg.id != s.q.id {
			return nil
		}
		s.busy = false
		if msg.result.err != nil {
			s.errMsg = msg.result.err.Error()
			return nil
		}
		if msg.result.apply != nil {
			msg.result.apply(&w.state)
		}
		return w.nextQuestion(s.q)

	case tea.PasteMsg:
		if !s.busy && s.focus.button() < 0 && len(s.inputs) > 0 {
			s.inputs[s.focus.at].insert(msg.Content)
			s.errMsg = ""
		}
		return nil

	case tea.KeyPressMsg:
		if s.busy {
			return nil
		}
		key := msg.String()
		if key == "esc" {
			return w.prevQuestion(s.q)
		}
		if key == "enter" {
			if s.focus.button() == buttonBack {
				return w.prevQuestion(s.q)
			}
			return s.submit(w)
		}
		if s.focus.handle(key) {
			return nil
		}
		if s.focus.button() >= 0 {
			return nil
		}
		if s.q.isChoice() {
			switch key {
			case "up", "k":
				s.cursor = max(0, s.cursor-1)
			case "down", "j":
				s.cursor = min(len(s.opts)-1, s.cursor+1)
			case "home":
				s.cursor = 0
			case "end":
				s.cursor = len(s.opts) - 1
			}
			s.errMsg = ""
			return nil
		}
		switch key {
		case "up":
			s.focus.at = max(0, s.focus.at-1)
			return nil
		case "down":
			if s.focus.at < len(s.inputs)-1 {
				s.focus.at++
			} else {
				s.focus.focusButton(buttonContinue)
			}
			return nil
		}
		if _, changed := s.inputs[s.focus.at].handleKey(msg); changed {
			// A stale error must not sit under a corrected value (#006).
			s.errMsg = ""
		}
	}
	return nil
}

func (s *questionScreen) frame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	spec := frameSpec{
		title:     s.q.step.title(),
		heading:   s.q.heading,
		explain:   s.q.explain(&w.state),
		focusLine: -1,
		errMsg:    s.errMsg,
		buttons:   []string{"Go Back", "Continue"},
		button:    s.focus.button(),
	}
	switch {
	case s.busy:
		spec.status = "Checking…"
	case s.notice != "":
		spec.status = s.notice
	}
	if s.q.isChoice() {
		focused := s.focus.button() < 0
		for i, o := range s.opts {
			marker := "  "
			label := sText.Render(o.label)
			if i == s.cursor {
				marker = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
				if focused {
					label = sSelected.Render(o.label)
				} else {
					label = sHighlight.Render(o.label)
				}
				spec.focusLine = len(spec.body)
			}
			spec.body = append(spec.body, marker+label)
			for _, l := range wrapText(o.detail, inner-4) {
				if l != "" {
					spec.body = append(spec.body, "    "+sMuted.Render(l))
				}
			}
		}
		if len(s.opts) == 0 {
			spec.body = append(spec.body, sMuted.Render("(nothing to choose from)"))
		}
		spec.keys = "↑/↓ choose · enter continue · tab buttons · esc back · ctrl+c quit"
		return spec
	}
	labelWidth := 0
	for _, f := range s.q.fields {
		labelWidth = max(labelWidth, lipgloss.Width(f))
	}
	fieldWidth := min(64, max(12, inner-labelWidth-2))
	for i, in := range s.inputs {
		focused := s.focus.at == i
		label := sMuted.Render(padTo(s.q.fields[i], labelWidth))
		if focused {
			label = lipgloss.NewStyle().Foreground(cPurple).Render(padTo(s.q.fields[i], labelWidth))
			spec.focusLine = len(spec.body)
		}
		placeholder := ""
		if i < len(s.q.placeholders) {
			placeholder = s.q.placeholders[i]
		}
		spec.body = append(spec.body, label+"  "+in.view(fieldWidth, focused, placeholder))
		if i < len(s.inputs)-1 {
			spec.body = append(spec.body, "")
		}
	}
	nav := ""
	if len(s.inputs) > 1 {
		nav = "↑/↓ fields · "
	}
	spec.keys = fmt.Sprintf("%s←/→ move cursor · enter continue · tab buttons · esc back · ctrl+c quit", nav)
	return spec
}
