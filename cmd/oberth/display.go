package main

import (
	"strings"
	"unicode"
)

func safeDisplay(text string) string {
	var out strings.Builder
	state := 0
	for _, r := range text {
		if state != 0 {
			state = terminalState(state, r)
			continue
		}
		switch r {
		case 0x1b:
			state = 1
		case 0x9b:
			state = 2
		case 0x9d:
			state = 3
		default:
			if r == '\n' || !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func terminalState(state int, r rune) int {
	switch state {
	case 1:
		state = 0
		if r == '[' {
			state = 2
		}
		if r == ']' {
			state = 3
		}
	case 2:
		if r >= 0x40 && r <= 0x7e {
			state = 0
		}
	case 3:
		return terminalOSC(r)
	case 4:
		state = 3
		if r == '\\' {
			state = 0
		}
	}
	return state
}

func terminalOSC(r rune) int {
	if r == 7 || r == 0x9c {
		return 0
	}
	if r == 0x1b {
		return 4
	}
	return 3
}
