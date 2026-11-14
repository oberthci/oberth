package model

import (
	"strings"
	"unicode"
)

// SanitizeNodeMessage projects untrusted Argo text into a bounded display value.
// Terminal escape sequences and control/format characters never reach readers.
func SanitizeNodeMessage(message string) string {
	const limit = 512
	var out strings.Builder
	state, count := "text", 0
	for _, r := range message {
		switch state {
		case "escape":
			switch r {
			case '[':
				state = "csi"
			case ']', 'P', 'X', '^', '_':
				state = "string"
			default:
				state = "text"
				if r >= 0x20 && r <= 0x2f {
					state = "intermediate"
				}
			}
		case "intermediate":
			if r >= 0x30 && r <= 0x7e {
				state = "text"
			}
		case "csi":
			if r >= 0x40 && r <= 0x7e {
				state = "text"
			}
		case "string":
			switch r {
			case '\a', 0x9c:
				state = "text"
			case '\x1b':
				state = "string-escape"
			}
		case "string-escape":
			state = "string"
			if r == '\\' {
				state = "text"
			}
		default:
			switch r {
			case '\x1b':
				state = "escape"
			case 0x9b:
				state = "csi"
			case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
				state = "string"
			default:
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
					out.WriteRune(r)
					count++
					if count == limit {
						return out.String()
					}
				}
			}
		}
	}
	return out.String()
}
